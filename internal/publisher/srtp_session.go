// Package publisher — SRTP session wrapper for HomeKit camera streaming.
//
// One srtpVideoSender per active streaming session, created when iOS
// hits the SelectedRTPStreamConfiguration "Start" command. Owns:
//   - the UDP socket to iOS (sender side, port from SetupEndpoints)
//   - the pion/srtp Session encrypting outgoing RTP
//   - the H264 packetizer that turns NAL units into RTP packets
//
// Lifecycle: Start spawns the goroutines, Stop cancels them and closes
// the socket. Send is the entry point for media samples.

package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/srtp/v3"
)

// srtpVideoSender encrypts H.264 NAL units into SRTP and sends them to
// the iOS controller's video RTP port.
type srtpVideoSender struct {
	logger *slog.Logger

	conn      net.Conn // UDP socket connected to iOS:videoRtpPort
	session   *srtp.SessionSRTP
	writer    *srtp.WriteStreamSRTP
	packetize *H264Packetizer

	// 90 kHz monotonic clock for RTP timestamps. Started when the
	// session begins; we offset every Send by the elapsed wall time.
	startTime  time.Time
	timestampOffset uint32 // initial random value, RFC 3550 §5.1

	// Stats — sampled by SendNAL, logged periodically by a separate
	// goroutine. Avoids one log line per NAL unit (~30/s) which floods
	// the camera SD.
	nalCount    uint64
	packetCount uint64
	byteCount   uint64
	lastLogged  time.Time

	// Per-NAL-type counts for debug. Indices = NAL type (5 bits).
	nalTypeCount [32]uint64

	// Debug: write a copy of every NAL (Annex B framed) to this file.
	// Set via SRTP_DEBUG_DUMP env var (path). Useful to validate the
	// stream we generate by playing it back with ffplay.
	debugDump *os.File

	mu sync.Mutex
	closed bool
}

// newSRTPVideoSender opens a UDP socket to the iOS controller and
// initialises the SRTP encryption context with the keys negotiated
// during SetupEndpoints.
//
// Parameters:
//   - controllerIP/port: iOS endpoint to send to
//   - localKey, localSalt: 16 + 14 bytes — keys WE chose, used to
//     encrypt outgoing packets
//   - remoteKey, remoteSalt: 16 + 14 bytes — iOS chose these. Required
//     by pion even though we don't read from this socket.
//   - ssrc: the SSRC we announced in SetupEndpointsResponse
//   - payloadType: dynamic RTP PT negotiated by HomeKit (typically 99)
func newSRTPVideoSender(
	controllerIP net.IP,
	controllerPort uint16,
	localKey, localSalt []byte,
	remoteKey, remoteSalt []byte,
	ssrc uint32,
	payloadType uint8,
	logger *slog.Logger,
) (*srtpVideoSender, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(localKey) != 16 || len(localSalt) != 14 {
		return nil, fmt.Errorf("srtp: bad local key/salt size (%d/%d)", len(localKey), len(localSalt))
	}
	if len(remoteKey) != 16 || len(remoteSalt) != 14 {
		return nil, fmt.Errorf("srtp: bad remote key/salt size (%d/%d)", len(remoteKey), len(remoteSalt))
	}

	// Open a *net.UDPConn already "connected" to iOS so writes go to
	// the right destination without sendto().
	udpConn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: controllerIP, Port: int(controllerPort)})
	if err != nil {
		return nil, fmt.Errorf("srtp: dial udp: %w", err)
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
		return nil, fmt.Errorf("srtp: new session: %w", err)
	}

	writer, err := session.OpenWriteStream()
	if err != nil {
		_ = session.Close()
		_ = udpConn.Close()
		return nil, fmt.Errorf("srtp: open write stream: %w", err)
	}

	pkter := NewH264Packetizer(ssrc)
	pkter.SetPayloadType(payloadType)

	logger.Info("srtp: video sender ready",
		"controller", fmt.Sprintf("%s:%d", controllerIP, controllerPort),
		"ssrc", ssrc,
		"pt", payloadType)

	// Optional debug dump of every NAL passed to SendNAL, in Annex B
	// format, to the path given by SRTP_DEBUG_DUMP. Truncate on open.
	var dump *os.File
	if path := os.Getenv("SRTP_DEBUG_DUMP"); path != "" {
		f, err := os.Create(path)
		if err == nil {
			dump = f
			logger.Info("srtp: debug dump enabled", "path", path)
		} else {
			logger.Warn("srtp: cannot open debug dump", "path", path, "error", err)
		}
	}

	return &srtpVideoSender{
		logger:          logger,
		conn:            udpConn,
		session:         session,
		writer:          writer,
		packetize:       pkter,
		startTime:       time.Now(),
		timestampOffset: 0, // could randomise per RFC 3550, not strictly required
		lastLogged:      time.Now(),
		debugDump:       dump,
	}, nil
}

// SendNAL packetizes one H.264 NAL unit (with its source PTS in 90 kHz
// units) and sends every resulting RTP packet (encrypted as SRTP) to
// iOS. The isLastOfAccessUnit flag tells the packetizer to set the RTP
// marker bit on the last RTP packet emitted — required by RFC 6184 to
// signal the end of a video frame. Caller is responsible for buffering
// one sample to know when an access unit boundary is crossed.
//
// CRUCIAL: every NAL unit belonging to the same video frame MUST be
// passed with the same PTS. iOS uses the RTP timestamp to group NAL
// units into access units, and inter-frame slices that share a
// timestamp with their IDR are reconstructed as a single decoded
// frame. Passing wall-clock timestamps that increase between every
// NAL of the same frame causes iOS to drop everything but the first
// slice — symptom: the camera tile shows a single still frame.
func (s *srtpVideoSender) SendNAL(nal []byte, pts90khz int64, isLastOfAccessUnit bool) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("srtp: sender closed")
	}
	s.mu.Unlock()

	// MPEG-TS PTS is already in 90 kHz units, the same clock as the
	// RTP video timestamp. Pass it through as-is (mod 2^32) plus an
	// optional random offset for RFC 3550 §5.1 compliance.
	var ts uint32
	if pts90khz >= 0 {
		ts = uint32(pts90khz) + s.timestampOffset
	} else {
		// Fallback to wall clock for samples that don't carry a PTS
		// (shouldn't happen with hlcamd, but defensive).
		elapsedNs := time.Since(s.startTime).Nanoseconds()
		ts = uint32(elapsedNs*90/1_000_000) + s.timestampOffset
	}
	s.packetize.SetTimestamp(ts)

	// Optional Annex B dump for offline validation.
	if s.debugDump != nil {
		_, _ = s.debugDump.Write([]byte{0, 0, 0, 1})
		_, _ = s.debugDump.Write(nal)
	}

	pkts := s.packetize.PacketizeWithMarker(nal, isLastOfAccessUnit)
	for _, pkt := range pkts {
		n, err := s.writer.WriteRTP(&pkt.Header, pkt.Payload)
		if err != nil {
			return fmt.Errorf("srtp: write rtp: %w", err)
		}
		s.packetCount++
		s.byteCount += uint64(n)
	}
	s.nalCount++
	if len(nal) > 0 {
		s.nalTypeCount[nal[0]&0x1F]++
	}

	// Periodic stats log: every 2 seconds, output one summary line.
	if time.Since(s.lastLogged) >= 2*time.Second {
		s.logger.Info("srtp: stream stats",
			"nals", s.nalCount,
			"packets", s.packetCount,
			"bytes", s.byteCount,
			"slice", s.nalTypeCount[1],
			"idr", s.nalTypeCount[5],
			"sei", s.nalTypeCount[6],
			"sps", s.nalTypeCount[7],
			"pps", s.nalTypeCount[8],
			"aud", s.nalTypeCount[9],
		)
		s.lastLogged = time.Now()
	}
	return nil
}

// Close shuts down the SRTP session and closes the UDP socket. Safe to
// call multiple times.
func (s *srtpVideoSender) Close() error {
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
	if s.debugDump != nil {
		_ = s.debugDump.Close()
		s.debugDump = nil
	}
	return nil
}

// _ unused context anchor for future extensions (sample timeouts etc.)
var _ = context.Canceled
