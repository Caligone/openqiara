// Package rtspserver exposes the camera's H.264 stream as a standard RTSP
// service for Scrypted, Frigate or VLC, from the media hub that feeds the
// HomeKit path too. With the multicast source, the microphone comes along
// as AAC-LC (RFC 3640), encoded here from the hub's PCM.
package rtspserver

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"

	"github.com/caligone/openqiara/internal/aacenc"
	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/mediahub"
)

// nalType returns the 5-bit H.264 NAL unit type from a NAL unit with no
// Annex B start code (camera.Sample's form).
func nalType(nal []byte) uint8 {
	if len(nal) == 0 {
		return 0
	}
	return nal[0] & 0x1f
}

const (
	nalTypeIDR = 5
	nalTypeSPS = 7
	nalTypePPS = 8
	nalTypeAUD = 9
)

// Config configures the RTSP server.
type Config struct {
	// Listen is the address the RTSP server binds to, e.g. ":8554".
	Listen string
	// Path is the RTSP stream path, e.g. "openqiara" →
	// rtsp://host:8554/openqiara.
	Path string
	// Audio adds the microphone, when the hub carries it (multicast
	// source) and libfdk-aac is at hand.
	Audio bool
}

// The microphone as RTSP serves it: AAC-LC 16 kHz mono, one 64 ms access
// unit per packet.
const (
	audioRate    = 16000
	audioBitrate = 32000
)

// Server serves a single H.264 stream over RTSP. The camera pipeline runs
// only while at least one client is reading, mirroring the on-demand
// behaviour of the HomeKit path so hlcamd isn't kept awake needlessly.
type Server struct {
	cfg  Config
	log  *slog.Logger
	hub  *mediahub.Hub
	rsrv *gortsplib.Server

	mu       sync.Mutex
	stream   *gortsplib.ServerStream
	forma    *format.H264
	audio    *format.MPEG4Audio // nil: video only
	desc     *description.Session
	readers  int
	cancelPL context.CancelFunc // cancels the running pipeline goroutine

	// randomStart offsets RTP timestamps per RFC 3550 §5.1; fixed for the
	// server's lifetime so all readers share a consistent timeline.
	randomStart uint32

	// sps/pps are cached from the stream so we can re-emit them before
	// each IDR for clients that joined mid-GOP.
	sps []byte
	pps []byte

	// dropped counts the packets dropped for slow readers since the last
	// summary, logged at most every dropLogEvery.
	dropped    atomic.Int64
	dropMu     sync.Mutex
	dropLogged time.Time
}

// dropLogEvery rate-limits the dropped packets summary: a slow reader
// drops hundreds of packets per second.
const dropLogEvery = 10 * time.Second

// New returns an RTSP server. The hub is the shared media pipeline the
// server subscribes to on the first reader; if nil, New panics — the RTSP
// server has no standalone pipeline anymore. Call Start to bind and serve.
func New(cfg Config, hub *mediahub.Hub, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Path == "" {
		cfg.Path = "openqiara"
	}
	return &Server{cfg: cfg, hub: hub, log: logger}
}

// Start binds the RTSP listener and begins serving. It returns once the
// listener is up; serving continues in the background until ctx is
// cancelled.
func (s *Server) Start(ctx context.Context) error {
	rs, err := randUint32()
	if err != nil {
		return fmt.Errorf("rtsp: random ts: %w", err)
	}
	s.randomStart = rs

	// H.264 format with a dynamic payload type. The camera emits SPS/PPS
	// in-band, so clients pick up parameter sets before the first IDR; we
	// also re-emit them ourselves ahead of each IDR (see writeAU).
	s.forma = &format.H264{
		PayloadTyp:        96,
		PacketizationMode: 1,
	}
	medi := &description.Media{
		Type:    description.MediaTypeVideo,
		Formats: []format.Format{s.forma},
	}
	s.desc = &description.Session{Medias: []*description.Media{medi}}
	if s.cfg.Audio {
		if enc, err := aacenc.New(aacenc.LC, audioRate, audioBitrate); err != nil {
			s.log.Warn("rtsp: no AAC encoder, video only", "error", err)
		} else {
			enc.Close()
			s.audio = &format.MPEG4Audio{
				PayloadTyp: 97,
				Config: &mpeg4audio.AudioSpecificConfig{
					Type: mpeg4audio.ObjectTypeAACLC, SampleRate: audioRate, ChannelConfig: 1,
				},
				SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3,
			}
			s.desc.Medias = append(s.desc.Medias, &description.Media{
				Type: description.MediaTypeAudio, Formats: []format.Format{s.audio},
			})
		}
	}

	s.rsrv = &gortsplib.Server{
		Handler:     s,
		RTSPAddress: s.cfg.Listen,
	}
	if err := s.rsrv.Start(); err != nil {
		return fmt.Errorf("rtsp listen %s: %w", s.cfg.Listen, err)
	}
	s.log.Info("rtsp server listening", "addr", s.cfg.Listen, "path", s.cfg.Path)

	go func() {
		<-ctx.Done()
		s.rsrv.Close()
	}()
	return nil
}

// --- gortsplib.ServerHandler ---

// OnStreamWriteError implements gortsplib.ServerHandlerOnStreamWriteError,
// called for each packet dropped because a reader's write queue is full.
// Without it gortsplib logs every single one, hundreds per second on a slow
// Wi-Fi link: count them and log a summary instead.
func (s *Server) OnStreamWriteError(ctx *gortsplib.ServerHandlerOnStreamWriteErrorCtx) {
	s.dropped.Add(1)
	s.dropMu.Lock()
	defer s.dropMu.Unlock()
	if time.Since(s.dropLogged) < dropLogEvery {
		return
	}
	s.dropLogged = time.Now()
	s.log.Warn("rtsp: reader too slow, packets dropped", "dropped", s.dropped.Swap(0), "error", ctx.Error)
}

// OnConnOpen implements gortsplib.ServerHandlerOnConnOpen.
func (s *Server) OnConnOpen(_ *gortsplib.ServerHandlerOnConnOpenCtx) {}

// OnConnClose implements gortsplib.ServerHandlerOnConnClose.
func (s *Server) OnConnClose(_ *gortsplib.ServerHandlerOnConnCloseCtx) {}

// OnSessionOpen implements gortsplib.ServerHandlerOnSessionOpen.
func (s *Server) OnSessionOpen(_ *gortsplib.ServerHandlerOnSessionOpenCtx) {}

// OnSessionClose implements gortsplib.ServerHandlerOnSessionClose. A
// closing session that was reading counts as a reader leaving.
func (s *Server) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	if ctx.Session.State() == gortsplib.ServerSessionStatePlay {
		s.onReaderGone()
	}
}

// OnDescribe advertises the single H.264 stream, creating it on demand.
func (s *Server) OnDescribe(_ *gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st, err := s.ensureStream()
	if err != nil {
		return &base.Response{StatusCode: base.StatusInternalServerError}, nil, err
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnSetup returns the shared stream for the requested media.
func (s *Server) OnSetup(_ *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	st, err := s.ensureStream()
	if err != nil {
		return &base.Response{StatusCode: base.StatusInternalServerError}, nil, err
	}
	return &base.Response{StatusCode: base.StatusOK}, st, nil
}

// OnPlay starts the camera pipeline for the first concurrent reader.
func (s *Server) OnPlay(_ *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	s.mu.Lock()
	s.readers++
	first := s.readers == 1
	s.mu.Unlock()
	if first {
		s.startPipeline()
	}
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// ensureStream lazily creates the shared ServerStream.
func (s *Server) ensureStream() (*gortsplib.ServerStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil {
		return s.stream, nil
	}
	st := &gortsplib.ServerStream{Server: s.rsrv, Desc: s.desc}
	if err := st.Initialize(); err != nil {
		return nil, fmt.Errorf("rtsp: stream init: %w", err)
	}
	s.stream = st
	return st, nil
}

// onReaderGone stops the pipeline once the last reader leaves.
func (s *Server) onReaderGone() {
	s.mu.Lock()
	if s.readers > 0 {
		s.readers--
	}
	last := s.readers == 0
	cancel := s.cancelPL
	if last {
		s.cancelPL = nil
	}
	s.mu.Unlock()

	if last && cancel != nil {
		cancel()
	}
}

// --- pipeline ---

// startPipeline launches the HLS→MPEG-TS→NAL→RTP goroutine.
func (s *Server) startPipeline() {
	plCtx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancelPL = cancel
	s.mu.Unlock()
	go s.runPipeline(plCtx)
}

// runPipeline subscribes to the shared media hub and packetizes its
// samples to RTP through gortsplib: NAL grouped into access units, PCM
// encoded to AAC-LC when the stream has audio.
func (s *Server) runPipeline(ctx context.Context) {
	enc := &rtph264.Encoder{
		PayloadType:       uint8(s.forma.PayloadTyp),
		PacketizationMode: 1,
	}
	if err := enc.Init(); err != nil {
		s.log.Error("rtsp: encoder init", "error", err)
		return
	}

	var audio *audioEncoder
	if s.audio != nil {
		a, err := newAudioEncoder(s.audio)
		if err != nil {
			s.log.Warn("rtsp: audio encoder", "error", err)
		} else {
			audio = a
			defer audio.close()
		}
	}

	sub := s.hub.Subscribe()
	defer sub.Close()

	// The parser emits one NAL per Sample. gortsplib's encoder wants a
	// whole access unit ([][]byte), so we buffer NAL of the same PTS and
	// flush the group when the PTS advances (or an AUD/new IDR marks a
	// boundary).
	s.log.Info("rtsp: pipeline started (via mediahub)", "audio", audio != nil)
	var au [][]byte
	var auPTS int64
	havePTS := false

	flush := func() {
		if len(au) == 0 {
			return
		}
		s.writeAU(enc, au, auPTS)
		au = au[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			s.log.Info("rtsp: pipeline stopped")
			return
		case sample, ok := <-sub.Samples():
			if !ok {
				flush()
				return
			}
			if !sample.IsVideo {
				if audio != nil {
					s.writeAudio(audio, sample)
				}
				continue
			}
			if havePTS && sample.PTS != auPTS {
				flush()
			}
			auPTS = sample.PTS
			havePTS = true
			au = append(au, append([]byte(nil), sample.Data...))
		}
	}
}

// writeAU caches parameter sets, prepends SPS/PPS ahead of an IDR for
// mid-GOP joiners, encodes the access unit to RTP and writes it to all
// readers.
func (s *Server) writeAU(enc *rtph264.Encoder, au [][]byte, pts90khz int64) {
	hasIDR := false
	for _, nal := range au {
		switch nalType(nal) {
		case nalTypeSPS:
			s.mu.Lock()
			s.sps = append(s.sps[:0], nal...)
			s.mu.Unlock()
		case nalTypePPS:
			s.mu.Lock()
			s.pps = append(s.pps[:0], nal...)
			s.mu.Unlock()
		case nalTypeIDR:
			hasIDR = true
		}
	}

	out := au
	if hasIDR {
		s.mu.Lock()
		sps, pps := s.sps, s.pps
		s.mu.Unlock()
		if len(sps) > 0 && len(pps) > 0 && !auHasParams(au) {
			out = append([][]byte{sps, pps}, au...)
		}
	}

	pkts, err := enc.Encode(out)
	if err != nil {
		s.log.Warn("rtsp: encode error", "error", err)
		return
	}
	ts := uint32(int64(s.randomStart) + pts90khz)
	s.mu.Lock()
	st := s.stream
	s.mu.Unlock()
	if st == nil {
		return
	}
	for _, p := range pkts {
		p.Timestamp = ts
		if err := st.WritePacketRTP(s.desc.Medias[0], p); err != nil {
			s.log.Warn("rtsp: write error", "error", err)
			return
		}
	}
}

// auHasParams reports whether the access unit already carries SPS and PPS
// (so we don't duplicate them).
func auHasParams(au [][]byte) bool {
	var sps, pps bool
	for _, nal := range au {
		switch nalType(nal) {
		case nalTypeSPS:
			sps = true
		case nalTypePPS:
			pps = true
		}
	}
	return sps && pps
}

// audioEncoder turns the hub's PCM into AAC-LC RTP packets.
type audioEncoder struct {
	aac *aacenc.Stream
	rtp *rtpmpeg4audio.Encoder
}

func newAudioEncoder(f *format.MPEG4Audio) (*audioEncoder, error) {
	aac, err := aacenc.NewStream(aacenc.LC, audioRate, audioBitrate)
	if err != nil {
		return nil, err
	}
	enc := &rtpmpeg4audio.Encoder{
		PayloadType: f.PayloadTyp, SizeLength: f.SizeLength,
		IndexLength: f.IndexLength, IndexDeltaLength: f.IndexDeltaLength,
	}
	if err := enc.Init(); err != nil {
		aac.Close()
		return nil, err
	}
	return &audioEncoder{aac: aac, rtp: enc}, nil
}

func (a *audioEncoder) close() { a.aac.Close() }

// writeAudio encodes the PCM and writes it to the readers. An access
// unit's RTP timestamp comes from the PTS of its first sample, on the same
// hlcamd clock as the video.
func (s *Server) writeAudio(a *audioEncoder, sample camera.Sample) {
	err := a.aac.Write(sample.Data, sample.PTS, func(au []byte, pts int64) {
		pkts, err := a.rtp.Encode([][]byte{au})
		if err != nil {
			s.log.Warn("rtsp: aac packetize", "error", err)
			return
		}
		s.mu.Lock()
		st := s.stream
		s.mu.Unlock()
		if st == nil {
			return
		}
		ts := uint32(int64(s.randomStart) + pts*audioRate/90000)
		for _, p := range pkts {
			p.Timestamp = ts
			if err := st.WritePacketRTP(s.desc.Medias[1], p); err != nil {
				s.log.Warn("rtsp: audio write error", "error", err)
				return
			}
		}
	})
	if err != nil {
		s.log.Warn("rtsp: aac encode", "error", err)
	}
}
