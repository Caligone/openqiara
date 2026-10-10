package publisher

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"

	"github.com/caligone/openqiara/internal/hksv"
)

// HomeKit Secure Video asks the camera accessory for more services
// (HAP-NodeJS CameraController.constructService):
//   - CameraRecordingManagement: what it can record, what iOS chose, and
//     whether recording is on;
//   - CameraOperatingMode: the user's camera switches in Home;
//   - DataStreamTransportManagement: how iOS opens the HomeKit Data Stream
//     the recordings travel on, linked to the recording service;
//   - MotionSensor: the trigger, fed by the person detection.
//
// Types, formats and permissions: HAP-NodeJS CharacteristicDefinitions.ts
// and ServiceDefinitions.ts.
const (
	typeCameraOperatingMode          = "21A"
	typeDataStreamTransportMgmt      = "129"
	typeRecordingAudioActive         = "226"
	typeHomeKitCameraActive          = "21B"
	typeEventSnapshotsActive         = "223"
	typePeriodicSnapshotsActive      = "225"
	typeSupportedDataStreamTransport = "130"
	typeSetupDataStreamTransport     = "131"
)

// recordingOffer is what the camera offers: a 4 s prebuffer, 4 s fragments
// (an IDR every 0.5 s starts each), HAP-NodeJS's example resolutions.
// hlcamd only makes 1920x1080 at 30 fps.
var recordingOffer = hksv.Offer{PrebufferMs: 4000, FragmentMs: 4000, Resolutions: [][3]int{
	{1920, 1080, 30}, {1600, 1200, 30}, {1280, 960, 30}, {1280, 720, 30}, {640, 480, 30},
	{640, 360, 30}, {480, 360, 30}, {480, 270, 30}, {320, 240, 30}, {320, 240, 15}, {320, 180, 30},
}}

// recording holds the camera's HKSV services.
type recording struct {
	log *slog.Logger

	// secret returns the Pair-Verify secret of the controller connected
	// from an address (hap.Server.SharedSecret, patched in third_party).
	secret func(addr string) ([32]byte, bool)
	stream *hksv.DataStream
	// store keeps what iOS wrote across restarts, as HAP-NodeJS does: iOS
	// doesn't write it again, and recordings are refused without it.
	store hap.Store

	recorder *hksv.Recorder
	dataSend *hksv.DataSend

	mgmt         *service.CameraRecordingManagement
	active       *characteristic.Active
	audioActive  *characteristic.Int
	cameraActive *characteristic.Int // HomeKitCameraActive
	setupStream  *characteristic.Bytes
	motion       *service.MotionSensor

	mu       sync.Mutex
	selected *hksv.Selected // nil until iOS chose
}

// Keys of the recording's state in hap's store.
const (
	storeActive      = "hksvActive"
	storeAudioActive = "hksvAudioActive"
	storeSelected    = "hksvSelected"
)

// switch01 is a 0/1 characteristic, as HomeKit's camera switches are.
func switch01(typ string, perms ...string) *characteristic.Int {
	c := characteristic.NewInt(typ)
	c.Format = characteristic.FormatUInt8
	c.Permissions = perms
	c.SetMinValue(0)
	c.SetMaxValue(1)
	c.ValidVals = []int{0, 1}
	_ = c.SetValue(1)
	return c
}

func newRecording(acc *accessory.Camera, logger *slog.Logger) *recording {
	r := &recording{log: logger}
	rw := []string{characteristic.PermissionRead, characteristic.PermissionWrite, characteristic.PermissionEvents}

	r.mgmt = service.NewCameraRecordingManagement()
	r.mgmt.SupportedCameraRecordingConfiguration.SetValue(recordingOffer.Camera())
	r.mgmt.SupportedVideoRecordingConfiguration.SetValue(recordingOffer.Video())
	r.mgmt.SupportedAudioRecordingConfiguration.SetValue(recordingOffer.Audio())
	r.active = characteristic.NewActive()
	r.mgmt.AddC(r.active.C)
	// HAP-NodeJS gives it timed write too, but hap v0.0.35 panics on a
	// timed-write characteristic written without a prepare
	// (characteristics.go:198, nil timedWr): the panic took the whole
	// request, live video setup included. Plain writes are accepted.
	r.audioActive = switch01(typeRecordingAudioActive, rw...)
	_ = r.audioActive.SetValue(0)
	r.mgmt.AddC(r.audioActive.C)

	mode := service.New(typeCameraOperatingMode)
	mode.AddC(switch01(typeEventSnapshotsActive, rw...).C)
	r.cameraActive = switch01(typeHomeKitCameraActive, rw...)
	mode.AddC(r.cameraActive.C)
	mode.AddC(switch01(typePeriodicSnapshotsActive, rw...).C)

	stream := service.New(typeDataStreamTransportMgmt)
	supported := characteristic.NewBytes(typeSupportedDataStreamTransport)
	supported.Format = characteristic.FormatTLV8
	supported.Permissions = []string{characteristic.PermissionRead}
	supported.SetValue(hksv.SupportedTransport())
	stream.AddC(supported.C)
	r.setupStream = characteristic.NewBytes(typeSetupDataStreamTransport)
	r.setupStream.Format = characteristic.FormatTLV8
	r.setupStream.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionWrite, characteristic.PermissionWriteResponse}
	r.setupStream.SetValue([]byte{})
	stream.AddC(r.setupStream.C)
	version := characteristic.NewVersion()
	version.SetValue("1.0")
	stream.AddC(version.C)
	r.mgmt.AddS(stream)

	// The trigger: linked to the recording service and active, as
	// HAP-NodeJS does; the home hub turned the camera down without it.
	r.motion = service.NewMotionSensor()
	statusActive := characteristic.NewStatusActive()
	statusActive.SetValue(true)
	r.motion.AddC(statusActive.C)
	r.mgmt.AddS(r.motion.S)

	// The live stream's own switch, which HAP-NodeJS gives every stream
	// management service of a recording camera.
	for _, sm := range []*service.CameraRTPStreamManagement{acc.StreamManagement1, acc.StreamManagement2} {
		streamActive := characteristic.NewActive()
		_ = streamActive.SetValue(characteristic.ActiveActive)
		sm.AddC(streamActive.C)
	}

	acc.AddS(r.mgmt.S)
	acc.AddS(mode)
	acc.AddS(stream)
	acc.AddS(r.motion.S)

	r.recorder = hksv.NewRecorder(recordingOffer.PrebufferMs, logger)
	r.dataSend = hksv.NewDataSend(r.openRecording, logger)
	r.mgmt.SelectedCameraRecordingConfiguration.OnValueRemoteUpdate(func(b []byte) {
		if r.selectConfig(b) {
			r.save(storeSelected, b)
		}
	})
	r.active.OnValueRemoteUpdate(func(v int) {
		r.log.Info("hksv: recording", "active", v == 1)
		r.recorder.SetActive(v == 1)
		r.save(storeActive, []byte{byte(v)})
	})
	r.audioActive.OnValueRemoteUpdate(func(v int) {
		r.log.Info("hksv: recording audio", "active", v == 1)
		r.save(storeAudioActive, []byte{byte(v)})
	})
	// The setup answer goes back in the write's response (permission wr).
	r.stream = hksv.NewDataStream(r.serveStream, logger)
	r.setupStream.SetValueRequestFunc = func(v any, req *http.Request) (any, int) {
		in, err := base64.StdEncoding.DecodeString(v.(string))
		if err != nil || req == nil || r.secret == nil {
			return nil, -70402
		}
		shared, ok := r.secret(req.RemoteAddr)
		if !ok {
			r.log.Warn("hksv: data stream setup from a controller with no session", "remote", req.RemoteAddr)
			return nil, -70402
		}
		out, err := r.stream.Setup(in, shared)
		if err != nil {
			r.log.Warn("hksv: data stream setup failed", "error", err)
			return nil, -70402
		}
		r.log.Info("hksv: data stream prepared", "controller", req.RemoteAddr)
		return base64.StdEncoding.EncodeToString(out), 0
	}
	return r
}

// selectConfig takes the configuration iOS selected; false if it can't
// be read.
func (r *recording) selectConfig(b []byte) bool {
	sel, err := hksv.ParseSelected(b)
	if err != nil {
		r.log.Warn("hksv: bad selected configuration", "error", err, "tlv", fmt.Sprintf("% x", b))
		return false
	}
	r.log.Info("hksv: recording configuration selected", "config", sel.String())
	r.mu.Lock()
	r.selected = &sel
	r.mu.Unlock()
	return true
}

// setStore restores what iOS wrote before a restart, and keeps what it
// writes next.
func (r *recording) setStore(st hap.Store) {
	r.store = st
	if b, err := st.Get(storeSelected); err == nil && r.selectConfig(b) {
		r.mgmt.SelectedCameraRecordingConfiguration.SetValue(b)
	}
	if b, err := st.Get(storeAudioActive); err == nil && len(b) == 1 {
		_ = r.audioActive.SetValue(int(b[0]))
	}
	if b, err := st.Get(storeActive); err == nil && len(b) == 1 {
		_ = r.active.SetValue(int(b[0]))
		r.recorder.SetActive(b[0] == 1)
	}
}

func (r *recording) save(key string, v []byte) {
	if r.store == nil {
		return
	}
	if err := r.store.Set(key, v); err != nil {
		r.log.Warn("hksv: state not saved", "key", key, "error", err)
	}
}

// serveStream handles one data stream connection from iOS: recordings.
func (r *recording) serveStream(c *hksv.Conn) {
	defer func() { _ = c.Close() }()
	r.dataSend.Serve(c)
}

// openRecording decides on a recording request, as HAP-NodeJS's
// handleDataSendOpen does: recording and the camera must be on, and iOS
// must have chosen a configuration.
func (r *recording) openRecording() (hksv.RecordFunc, int) {
	if r.active.Value() != characteristic.ActiveActive || r.cameraActive.Value() != 1 {
		return nil, hksv.ReasonNotAllowed
	}
	r.mu.Lock()
	sel := r.selected
	r.mu.Unlock()
	if sel == nil {
		return nil, hksv.ReasonInvalidConfig
	}
	o := hksv.Options{
		FragmentMs: sel.FragmentMs,
		// The microphone is AAC-LC 16 kHz or nothing.
		Audio:     r.audioActive.Value() == 1 && sel.AudioCodec == hksv.AudioAACLC && sel.SampleRate == hksv.SampleRate16kHz,
		AudioKbps: sel.AudioMaxKbps,
	}
	return func(ctx context.Context, emit func([]byte, bool) error) error {
		return r.recorder.Record(ctx, o, emit)
	}, 0
}

// setMotion reports motion in front of the camera, HKSV's trigger.
func (r *recording) setMotion(on bool) {
	r.motion.MotionDetected.SetValue(on)
}
