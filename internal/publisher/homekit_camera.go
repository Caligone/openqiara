package publisher

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/rtp"
	"github.com/brutella/hap/service"
	"github.com/brutella/hap/tlv8"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/mediahub"
)

// CameraConfig holds the configuration for the HomeKit Camera accessory.
// This is part of HomeKitConfig but kept as a separate struct for clarity.
type CameraConfig struct {
	// Enabled toggles the camera accessory entirely. When false, no
	// Camera service is added to the bridge.
	Enabled bool `json:"enabled,omitempty"`

	// Name shown next to the camera in the iOS Home app. Defaults to
	// "Caméra OpenQiara" if empty.
	Name string `json:"name,omitempty"`
}

// HomeKitCamera wraps the HAP camera accessory and tracks streaming
// sessions opened by paired iOS controllers. There is normally one
// HomeKitCamera per HomeKitPublisher; the publisher creates it during
// Start when CameraConfig.Enabled is true.
//
// HomeKitCamera is safe for concurrent use. iOS may issue multiple
// Setup/SelectStream commands from different connections.
type HomeKitCamera struct {
	cfg CameraConfig
	log *slog.Logger
	acc *accessory.Camera

	// hub est le pipeline média partagé (SetMediaHub) : chaque session
	// s'y abonne. Il réveille hlcamd si son flux s'est figé.
	hub *mediahub.Hub

	rec *recording // HomeKit Secure Video services

	mu       sync.Mutex
	sessions map[string]*cameraSession // keyed by session id (base64)
}

// SetMediaHub attache le hub média partagé. Doit être appelé avant la
// première session HK : sans lui, pas de flux.
func (c *HomeKitCamera) SetMediaHub(h *mediahub.Hub) {
	c.hub = h
	c.rec.recorder.SetHub(h)
}

// replace takes over from the camera of a previous build: its media hub,
// set once at startup, and its recording, stopped so that it lets go of
// the hub and its data stream port.
func (c *HomeKitCamera) replace(prev *HomeKitCamera) {
	prev.rec.recorder.SetActive(false)
	prev.rec.stream.Close()
	if prev.hub != nil {
		c.SetMediaHub(prev.hub)
	}
}

// cameraSession tracks the state of a single live-streaming session
// negotiated through SetupEndpoints + SelectedRTPStreamConfiguration.
// One session per active iOS viewer.
type cameraSession struct {
	id []byte // session identifier echoed back by iOS

	// Controller (iOS) endpoint
	controllerIP   net.IP
	videoRtpPort   uint16
	audioRtpPort   uint16
	controllerSRTP rtp.CryptoSuite // keys we use to *decrypt* packets from iOS
	audioCryptoIn  rtp.CryptoSuite

	// Accessory (us) endpoint — what we tell iOS in the response
	accessoryIP    net.IP
	accessorySRTP  rtp.CryptoSuite // keys we use to *encrypt* packets we send
	audioCryptoOut rtp.CryptoSuite
	ssrcVideo      int32
	ssrcAudio      int32

	// Negotiated RTP payload type from the FIRST Start command. iOS
	// sometimes omits this field on subsequent Reconfigure messages
	// (sends PT=0 which would corrupt our stream), so we cache the
	// initial value and reuse it across reconfigures.
	videoPT uint8
	audioPT uint8

	// Streaming pipeline (filled when SelectedRTP Start fires).
	cancel      context.CancelFunc // cancels the streaming goroutines
	sender      *srtpVideoSender
	audioSender *srtpAudioSender
	wg          sync.WaitGroup
	streaming   bool
}

// NewHomeKitCamera builds the HomeKit camera accessory and wires its
// SetupEndpoints / SelectedRTPStreamConfiguration handlers. The returned
// accessory.Camera should be added to the HAP bridge by the caller.
func NewHomeKitCamera(cfg CameraConfig, logger *slog.Logger) *HomeKitCamera {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Name == "" {
		cfg.Name = "Caméra OpenQiara"
	}

	cam := &HomeKitCamera{
		cfg:      cfg,
		log:      logger,
		sessions: make(map[string]*cameraSession),
	}

	cam.acc = accessory.NewCamera(accessory.Info{
		Name:         cfg.Name,
		Manufacturer: "OpenQiara",
		Model:        "Qiara Camera",
		Firmware:     "0.1.0",
	})

	cam.rec = newRecording(cam.acc, logger)

	// Publish the supported codec configurations once. iOS reads these
	// to know what we can do (resolutions, codecs, framerates).
	cam.publishStaticConfigs()

	// Wire the dynamic handlers.
	for _, sm := range cam.streams() {
		sm.SetupEndpoints.OnValueRemoteUpdate(func(b []byte) { cam.onSetupEndpoints(sm, b) })
		sm.SelectedRTPStreamConfiguration.OnValueRemoteUpdate(cam.onSelectedRTPStreamConfiguration)
	}

	return cam
}

// serveSnapshot answers POST /resource with the camera's next JPEG
// (hlcamd's 1280x720, one a second). Home asks for a size; it scales what
// it gets.
func (c *HomeKitCamera) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	jpeg, err := camera.MulticastSnapshot(ctx, camera.MulticastJPEG, c.log)
	if err != nil {
		c.log.Debug("camera: no snapshot", "error", err)
		http.Error(w, "", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	_, _ = w.Write(jpeg)
}

// setSessionSecrets gives the camera access to the HAP sessions' secrets,
// which HomeKit Secure Video's data stream derives its keys from.
func (c *HomeKitCamera) setSessionSecrets(secret func(addr string) ([32]byte, bool)) {
	c.rec.secret = secret
}

// SetMotion reports whether someone is in front of the camera (person or
// pet detection): the camera's motion sensor, which triggers HomeKit
// Secure Video.
func (c *HomeKitCamera) SetMotion(on bool) {
	c.rec.setMotion(on)
}

// Accessory returns the underlying HAP accessory so the publisher can
// add it to the bridge.
func (c *HomeKitCamera) Accessory() *accessory.Camera {
	return c.acc
}

// streams are the camera's two RTP stream management services: HomeKit
// wants at least two (HAP-NodeJS: "HomeKit requires at least 2 streams").
// Sessions are keyed by their id, whichever service set them up.
func (c *HomeKitCamera) streams() []*service.CameraRTPStreamManagement {
	return []*service.CameraRTPStreamManagement{c.acc.StreamManagement1, c.acc.StreamManagement2}
}

// publishStaticConfigs marshals the supported video, audio and crypto
// configurations into the corresponding read-only characteristics.
func (c *HomeKitCamera) publishStaticConfigs() {
	for _, sm := range c.streams() {
		c.publishStreamConfigs(sm)
	}
}

func (c *HomeKitCamera) publishStreamConfigs(sm *service.CameraRTPStreamManagement) {
	if b, err := tlv8.Marshal(rtp.DefaultVideoStreamConfiguration()); err == nil {
		sm.SupportedVideoStreamConfiguration.SetValue(b)
	} else {
		c.log.Error("camera: marshal video config", "error", err)
	}

	// AAC-ELD 16 kHz only, the microphone's rate (SendPCM): iOS picks
	// AAC-ELD even when offered nothing else (2026-10-10).
	audioCfg := rtp.AudioStreamConfiguration{Codecs: []rtp.AudioCodecConfiguration{
		rtp.NewAacEldAudioCodecConfiguration(),
	}}
	if b, err := tlv8.Marshal(audioCfg); err == nil {
		sm.SupportedAudioStreamConfiguration.SetValue(b)
	} else {
		c.log.Error("camera: marshal audio config", "error", err)
	}

	if b, err := tlv8.Marshal(rtp.NewConfiguration(rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80)); err == nil {
		sm.SupportedRTPConfiguration.SetValue(b)
	} else {
		c.log.Error("camera: marshal rtp config", "error", err)
	}

	// Streaming status: 0 = available
	if b, err := tlv8.Marshal(rtp.StreamingStatus{Status: rtp.StreamingStatusAvailable}); err == nil {
		sm.StreamingStatus.SetValue(b)
	} else {
		c.log.Error("camera: marshal streaming status", "error", err)
	}
}

// onSetupEndpoints handles a write to the SetupEndpoints characteristic.
// iOS writes its own IP, ports and SRTP keys, then expects us to write
// back our IP, ports and SRTP keys (round-trip).
func (c *HomeKitCamera) onSetupEndpoints(sm *service.CameraRTPStreamManagement, buf []byte) {
	var req rtp.SetupEndpoints
	if err := tlv8.Unmarshal(buf, &req); err != nil {
		c.log.Error("camera: unmarshal setup endpoints", "error", err)
		return
	}

	c.log.Info("camera: setup endpoints from iOS",
		"session", base64.StdEncoding.EncodeToString(req.SessionId),
		"controller_ip", req.ControllerAddr.IPAddr,
		"video_port", req.ControllerAddr.VideoRtpPort,
		"audio_port", req.ControllerAddr.AudioRtpPort)

	// Generate our SRTP master keys (16 bytes key + 14 bytes salt) for
	// both directions. iOS uses req.Video / req.Audio to encrypt the
	// (rare) packets it sends to us. We use accessorySRTP to encrypt
	// what we send to iOS.
	accVideo, err := newCryptoSuite(req.Video.Type)
	if err != nil {
		c.log.Error("camera: generate accessory video crypto", "error", err)
		return
	}
	accAudio, err := newCryptoSuite(req.Audio.Type)
	if err != nil {
		c.log.Error("camera: generate accessory audio crypto", "error", err)
		return
	}

	// Pick our IP from the same address family as iOS asked for.
	var accIP net.IP
	if req.ControllerAddr.IPVersion == rtp.IPAddrVersionv4 {
		accIP = pickIPv4()
	}
	if accIP == nil {
		c.log.Error("camera: no usable accessory IP")
		return
	}

	// Random SSRCs for the two streams.
	ssrcVideo := randomInt32()
	ssrcAudio := randomInt32()

	sess := &cameraSession{
		id:             req.SessionId,
		controllerIP:   net.ParseIP(req.ControllerAddr.IPAddr),
		videoRtpPort:   req.ControllerAddr.VideoRtpPort,
		audioRtpPort:   req.ControllerAddr.AudioRtpPort,
		controllerSRTP: req.Video,
		audioCryptoIn:  req.Audio,
		accessoryIP:    accIP,
		accessorySRTP:  accVideo,
		audioCryptoOut: accAudio,
		ssrcVideo:      ssrcVideo,
		ssrcAudio:      ssrcAudio,
	}

	c.mu.Lock()
	c.sessions[string(req.SessionId)] = sess
	c.mu.Unlock()

	resp := rtp.SetupEndpointsResponse{
		SessionId: req.SessionId,
		Status:    rtp.SessionStatusSuccess,
		AccessoryAddr: rtp.Addr{
			IPVersion:    req.ControllerAddr.IPVersion,
			IPAddr:       accIP.String(),
			VideoRtpPort: req.ControllerAddr.VideoRtpPort, // we send back same port
			AudioRtpPort: req.ControllerAddr.AudioRtpPort,
		},
		Video:     accVideo,
		Audio:     accAudio,
		SsrcVideo: ssrcVideo,
		SsrcAudio: ssrcAudio,
	}

	respBytes, err := tlv8.Marshal(resp)
	if err != nil {
		c.log.Error("camera: marshal setup response", "error", err)
		return
	}
	sm.SetupEndpoints.SetValue(respBytes)

	c.log.Info("camera: setup endpoints reply sent",
		"session", base64.StdEncoding.EncodeToString(req.SessionId),
		"accessory_ip", accIP.String(),
		"ssrc_video", ssrcVideo)
}

// onSelectedRTPStreamConfiguration handles a write to the
// SelectedRTPStreamConfiguration characteristic. iOS uses this to start,
// suspend, resume, reconfigure or end a streaming session previously
// negotiated through SetupEndpoints.
func (c *HomeKitCamera) onSelectedRTPStreamConfiguration(buf []byte) {
	var req rtp.StreamConfiguration
	if err := tlv8.Unmarshal(buf, &req); err != nil {
		c.log.Error("camera: unmarshal selected stream config", "error", err)
		return
	}

	sessionKey := string(req.Command.Identifier)

	c.mu.Lock()
	sess, ok := c.sessions[sessionKey]
	c.mu.Unlock()

	if !ok {
		c.log.Warn("camera: selected stream config for unknown session",
			"session", base64.StdEncoding.EncodeToString(req.Command.Identifier),
			"command", req.Command.Type)
		return
	}

	switch req.Command.Type {
	case rtp.SessionControlCommandTypeStart:
		c.log.Info("camera: stream START requested",
			"session", base64.StdEncoding.EncodeToString(sess.id),
			"width", req.Video.Attributes.Width,
			"height", req.Video.Attributes.Height,
			"fps", req.Video.Attributes.Framerate,
			"video_payload_type", req.Video.RTP.PayloadType,
			"audio_codec", req.Audio.CodecType,
			"audio_rate", req.Audio.CodecParams.Samplerate,
			"audio_kbps", req.Audio.RTP.Bitrate)
		if req.Audio.CodecType != rtp.AudioCodecType_AAC_ELD {
			c.log.Warn("camera: iOS chose an audio codec we don't send, no sound", "codec", req.Audio.CodecType)
		}
		if err := c.startStreaming(sess, req.Video.RTP.PayloadType, req.Audio.RTP.PayloadType); err != nil {
			c.log.Error("camera: failed to start streaming", "error", err)
		}

	case rtp.SessionControlCommandTypeEnd:
		c.log.Info("camera: stream END", "session", base64.StdEncoding.EncodeToString(sess.id))
		c.stopStreaming(sess)
		c.mu.Lock()
		delete(c.sessions, sessionKey)
		c.mu.Unlock()

	case rtp.SessionControlCommandTypeSuspend:
		c.log.Info("camera: stream SUSPEND", "session", base64.StdEncoding.EncodeToString(sess.id))
		c.stopStreaming(sess)

	case rtp.SessionControlCommandTypeResume:
		c.log.Info("camera: stream RESUME", "session", base64.StdEncoding.EncodeToString(sess.id))
		if err := c.startStreaming(sess, req.Video.RTP.PayloadType, req.Audio.RTP.PayloadType); err != nil {
			c.log.Error("camera: failed to resume streaming", "error", err)
		}

	case rtp.SessionControlCommandTypeReconfigure:
		c.log.Info("camera: stream RECONFIGURE", "session", base64.StdEncoding.EncodeToString(sess.id))
		c.stopStreaming(sess)
		if err := c.startStreaming(sess, req.Video.RTP.PayloadType, req.Audio.RTP.PayloadType); err != nil {
			c.log.Error("camera: failed to reconfigure streaming", "error", err)
		}

	default:
		c.log.Warn("camera: unknown stream command", "type", req.Command.Type)
	}
}

// startStreaming spawns the HLS watcher → MPEG-TS parser → H.264 RTP
// packetizer → SRTP sender pipeline for the given session. All
// goroutines are tied to a context that's cancelled by stopStreaming.
//
// videoPT and audioPT are the RTP payload types iOS just announced.
// We only honour them if non-zero — iOS sometimes sends PT=0 on
// Reconfigure which would be interpreted as µ-law audio and break
// decoding. In that case we fall back to the cached PT from the
// original Start.
func (c *HomeKitCamera) startStreaming(sess *cameraSession, videoPT, audioPT uint8) error {
	if sess.streaming {
		c.log.Warn("camera: session already streaming, ignoring start")
		return nil
	}
	if c.hub == nil {
		return fmt.Errorf("camera: no media hub")
	}
	payloadType := videoPT
	if payloadType == 0 {
		payloadType = sess.videoPT
		c.log.Info("camera: video PT=0 from iOS, reusing cached", "pt", payloadType)
	} else {
		sess.videoPT = payloadType
	}
	if payloadType == 0 {
		payloadType = 99
		sess.videoPT = 99
		c.log.Warn("camera: no video payload type known, defaulting to 99")
	}
	apt := audioPT
	if apt == 0 {
		apt = sess.audioPT
	} else {
		sess.audioPT = apt
	}
	if apt == 0 {
		apt = audioDefaultPayloadType
		sess.audioPT = apt
	}

	// Build SRTP video sender. localKey/salt = ours (we encrypt outgoing),
	// remoteKey/salt = iOS's (pion needs them even though we don't read).
	sender, err := newSRTPVideoSender(
		sess.controllerIP,
		sess.videoRtpPort,
		sess.accessorySRTP.MasterKey,
		sess.accessorySRTP.MasterSalt,
		sess.controllerSRTP.MasterKey,
		sess.controllerSRTP.MasterSalt,
		uint32(sess.ssrcVideo),
		payloadType,
		c.log,
	)
	if err != nil {
		return fmt.Errorf("srtp sender: %w", err)
	}
	sess.sender = sender

	// Build SRTP audio sender — sends Opus silence frames at 50 fps to
	// the iOS audio port. iOS rejects HomeKit camera sessions that
	// don't deliver an audio stream alongside the video, even when
	// the actual content is silence.
	audioSender, err := newSRTPAudioSender(
		sess.controllerIP,
		sess.audioRtpPort,
		sess.audioCryptoOut.MasterKey,
		sess.audioCryptoOut.MasterSalt,
		sess.audioCryptoIn.MasterKey,
		sess.audioCryptoIn.MasterSalt,
		uint32(sess.ssrcAudio),
		apt,
		c.log,
	)
	if err != nil {
		_ = sender.Close()
		return fmt.Errorf("srtp audio sender: %w", err)
	}
	sess.audioSender = audioSender

	ctx, cancel := context.WithCancel(context.Background())
	sess.cancel = cancel
	sess.streaming = true

	// Audio silence pump — iOS rejects camera sessions without an
	// audio stream: silence fills in while no PCM comes.
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		audioSender.RunSilence(ctx)
	}()

	sub := c.hub.Subscribe()
	sampleCh := sub.Samples()
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()
		<-ctx.Done()
		sub.Close()
	}()

	// Samples → SRTP senders: video NAL by NAL, audio encoded to AAC-ELD.
	//
	// We buffer one video sample so we can decide if the CURRENT sample
	// is the last NAL of its access unit by peeking at the NEXT sample:
	// when the next sample's PTS differs from the current one's, the
	// current sample carries the marker bit (RFC 6184 §5.1: marker = 1
	// on the very last RTP packet of an access unit).
	sess.wg.Add(1)
	go func() {
		defer sess.wg.Done()

		var pending camera.Sample
		havePending := false

		flushPending := func(isLastOfAU bool) {
			if !havePending {
				return
			}
			if err := sender.SendNAL(pending.Data, pending.PTS, isLastOfAU); err != nil {
				c.log.Warn("camera: srtp send failed", "error", err)
			}
			havePending = false
		}

		for {
			select {
			case <-ctx.Done():
				flushPending(true)
				return
			case sample, ok := <-sampleCh:
				if !ok {
					flushPending(true)
					return
				}
				if !sample.IsVideo {
					if err := audioSender.SendPCM(sample.Data); err != nil {
						c.log.Debug("srtp audio: send failed", "error", err)
					}
					continue
				}
				if havePending {
					flushPending(pending.PTS != sample.PTS)
				}
				pending = sample
				havePending = true
			}
		}
	}()

	c.log.Info("camera: streaming pipeline started",
		"session", base64.StdEncoding.EncodeToString(sess.id))
	return nil
}

// stopStreaming cancels the pipeline goroutines for the given session
// and closes the SRTP senders. Idempotent.
func (c *HomeKitCamera) stopStreaming(sess *cameraSession) {
	if !sess.streaming {
		return
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.wg.Wait()
	if sess.sender != nil {
		_ = sess.sender.Close()
		sess.sender = nil
	}
	if sess.audioSender != nil {
		_ = sess.audioSender.Close()
		sess.audioSender = nil
	}
	sess.streaming = false
	c.log.Info("camera: streaming pipeline stopped",
		"session", base64.StdEncoding.EncodeToString(sess.id))
}

// newCryptoSuite generates a fresh SRTP master key and salt for the given
// suite type. AES_CM_128 wants 16+14 bytes, AES_256 wants 32+14.
func newCryptoSuite(suiteType byte) (rtp.CryptoSuite, error) {
	var keyLen int
	switch suiteType {
	case rtp.CryptoSuite_AES_CM_128_HMAC_SHA1_80:
		keyLen = 16
	case rtp.CryptoSuite_AES_256_CM_HMAC_SHA1_80:
		keyLen = 32
	default:
		return rtp.CryptoSuite{}, fmt.Errorf("unsupported crypto suite type %d", suiteType)
	}
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return rtp.CryptoSuite{}, err
	}
	salt := make([]byte, 14)
	if _, err := rand.Read(salt); err != nil {
		return rtp.CryptoSuite{}, err
	}
	return rtp.CryptoSuite{Type: suiteType, MasterKey: key, MasterSalt: salt}, nil
}

// pickIPv4 returns the first non-loopback IPv4 address on the host,
// preferring the ssv0 interface (camera WiFi).
func pickIPv4() net.IP {
	if iface, err := net.InterfaceByName("ssv0"); err == nil {
		if addrs, err := iface.Addrs(); err == nil {
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.To4() != nil {
					return ipnet.IP
				}
			}
		}
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
			continue
		}
		return ipnet.IP
	}
	return nil
}

// randomInt32 returns a random non-negative int32 in [0, 2^31). We
// avoid the sign bit because iOS' RFC 3550 SSRC parser treats the
// 32-bit field as unsigned, while brutella/hap's TLV8 serialiser uses
// int32; mixing signedness around the wire is asking for trouble.
func randomInt32() int32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return int32(b[0]&0x7F)<<24 | int32(b[1])<<16 | int32(b[2])<<8 | int32(b[3])
}
