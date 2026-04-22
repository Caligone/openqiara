// Package publisher — SRTP audio session for HomeKit camera streaming.
//
// HomeKit cameras require both a video AND an audio RTP stream in the
// session — iOS rejects sessions that only deliver video. This sender
// supports two modes:
//
//  1. SendAACFrame: ship real AAC-LC frames extracted from the hlcamd
//     MPEG-TS, packetized per RFC 3640 (mpeg4-generic). HomeKit
//     officially expects AAC-ELD but the iOS AudioToolbox decoder is
//     multi-profile and may accept AAC-LC.
//
//  2. RunSilence: pump hard-coded Opus silence frames at 50 fps,
//     useful as a fallback when no real audio source is available
//     (which is what we did before SendAACFrame existed).

package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/srtp/v3"
)

// makeAudioRTPHeader builds an RTP header for one Opus audio packet.
// Marker bit is always set on audio packets in HomeKit (each packet is
// a complete frame, no fragmentation needed for 20 ms Opus at 24 kHz).
func makeAudioRTPHeader(pt uint8, seq uint16, ts, ssrc uint32) *rtp.Header {
	return &rtp.Header{
		Version:        2,
		PayloadType:    pt,
		SequenceNumber: seq,
		Timestamp:      ts,
		SSRC:           ssrc,
		Marker:         true,
	}
}

// audioPayloadType is the dynamic RTP payload type negotiated by HomeKit
// for the audio stream. iOS sends it via SelectedRTPStreamConfiguration;
// we cache it and reuse on Reconfigure (same trick as video PT).
const audioDefaultPayloadType = 110

// opusSilencePacket is a single Opus packet representing 20 ms of
// silence at 24 kHz mono. The TOC byte 0xF8 = config 31 (CELT, 24 kHz,
// 20 ms) + s=0 (mono) + c=0 (1 frame). The frame data is empty for
// pure silence — Opus represents silence as the TOC byte alone.
var opusSilencePacket = []byte{0xF8}

// srtpAudioSender encrypts hard-coded Opus silence frames into SRTP and
// sends them to the iOS controller's audio RTP port at 50 fps.
type srtpAudioSender struct {
	logger *slog.Logger

	conn      net.Conn // UDP socket connected to iOS:audioRtpPort
	session   *srtp.SessionSRTP
	writer    *srtp.WriteStreamSRTP

	ssrc        uint32
	payloadType uint8

	// Sequence number / timestamp for synthetic Opus packets.
	mu     sync.Mutex
	seq    uint16
	tsBase uint32 // initial timestamp (random per RFC 3550)
	closed bool
}

// newSRTPAudioSender opens a UDP socket to the iOS controller and
// initialises the SRTP encryption context for the audio stream. Same
// shape as newSRTPVideoSender but on the audio port and with audio
// crypto keys (negotiated separately by SetupEndpoints).
func newSRTPAudioSender(
	controllerIP net.IP,
	controllerPort uint16,
	localKey, localSalt []byte,
	remoteKey, remoteSalt []byte,
	ssrc uint32,
	payloadType uint8,
	logger *slog.Logger,
) (*srtpAudioSender, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(localKey) != 16 || len(localSalt) != 14 {
		return nil, fmt.Errorf("srtp audio: bad local key/salt size (%d/%d)", len(localKey), len(localSalt))
	}
	if len(remoteKey) != 16 || len(remoteSalt) != 14 {
		return nil, fmt.Errorf("srtp audio: bad remote key/salt size (%d/%d)", len(remoteKey), len(remoteSalt))
	}
	if payloadType == 0 {
		payloadType = audioDefaultPayloadType
	}

	udpConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: controllerIP, Port: int(controllerPort)})
	if err != nil {
		return nil, fmt.Errorf("srtp audio: dial udp: %w", err)
	}

	cfg := &srtp.Config{
		Keys: srtp.SessionKeys{
			LocalMasterKey:   localKey,
			LocalMasterSalt:  localSalt,
			RemoteMasterKey:  remoteKey,
			RemoteMasterSalt: remoteSalt,
		},
		Profile: srtp.ProtectionProfileAes128CmHmacSha1_80,
	}

	session, err := srtp.NewSessionSRTP(udpConn, cfg)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("srtp audio: new session: %w", err)
	}

	writer, err := session.OpenWriteStream()
	if err != nil {
		_ = session.Close()
		_ = udpConn.Close()
		return nil, fmt.Errorf("srtp audio: open write stream: %w", err)
	}

	logger.Info("srtp audio: sender ready",
		"controller", fmt.Sprintf("%s:%d", controllerIP, controllerPort),
		"ssrc", ssrc,
		"pt", payloadType)

	return &srtpAudioSender{
		logger:      logger,
		conn:        udpConn,
		session:     session,
		writer:      writer,
		ssrc:        ssrc,
		payloadType: payloadType,
		seq:         uint16(time.Now().UnixNano() & 0xFFFF),
		tsBase:      uint32(time.Now().UnixNano() & 0xFFFFFFFF),
	}, nil
}

// RunSilence streams Opus silence packets to the iOS audio port at 50
// fps (one packet every 20 ms) until ctx is cancelled. Each packet
// advances the RTP timestamp by 480 samples (20 ms at 24 kHz). Used
// as a fallback when no real audio source is wired in.
func (s *srtpAudioSender) RunSilence(ctx context.Context) {
	const frameDuration = 20 * time.Millisecond
	const samplesPerFrame = 480 // 24 kHz * 20 ms

	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()

	tsOffset := uint32(0)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				return
			}
			seq := s.seq
			s.seq++
			s.mu.Unlock()

			ts := s.tsBase + tsOffset
			tsOffset += samplesPerFrame

			header := makeAudioRTPHeader(s.payloadType, seq, ts, s.ssrc)
			if _, err := s.writer.WriteRTP(header, opusSilencePacket); err != nil {
				s.logger.Debug("srtp audio: write failed", "error", err)
				return
			}
		}
	}
}

// SendAACFrame packetizes one raw AAC frame per RFC 3640 mpeg4-generic
// and sends it to iOS over SRTP. pts90khz is the source MPEG-TS PTS in
// 90 kHz units; we map it to a monotonic 16 kHz timestamp (the AAC
// sample rate).
//
// CURRENTLY UNUSED. iOS rejects raw AAC-LC silently (see aac_rtp.go
// header comment). Kept around for the future CGo libfdk-aac path
// that will produce real AAC-ELD frames; this packetization layer is
// reusable as long as the input is the right AAC profile.
func (s *srtpAudioSender) SendAACFrame(aacRaw []byte, pts90khz int64) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	seq := s.seq
	s.seq++
	s.mu.Unlock()

	// Map 90 kHz PTS to 16 kHz audio clock (the AAC sample rate from
	// hlcamd). RTP timestamp scaling = ts90 * audio_rate / 90000.
	var ts uint32
	if pts90khz >= 0 {
		ts = s.tsBase + uint32(pts90khz*16000/90000)
	} else {
		ts = s.tsBase
	}

	pkt := packetizeAAC(aacRaw, s.ssrc, seq, ts, s.payloadType)
	if pkt == nil {
		return nil
	}
	if _, err := s.writer.WriteRTP(&pkt.Header, pkt.Payload); err != nil {
		return err
	}
	return nil
}

// Close shuts down the SRTP session and closes the UDP socket.
func (s *srtpAudioSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.session != nil {
		_ = s.session.Close()
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
	return nil
}
