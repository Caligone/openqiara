package hksv

import (
	"errors"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"

	"github.com/caligone/openqiara/internal/aacenc"
	"github.com/caligone/openqiara/internal/camera"
)

// A recording is fragmented MP4 (the container iOS selects): an
// initialization segment (ftyp+moov) then fragments (moof+mdat), each
// starting at an IDR and lasting about the selected fragment length.
// hlcamd's H.264 is Constrained Baseline: no B-frames, decode order is
// presentation order.

const (
	videoTrack = 1
	audioTrack = 2

	videoClock = 90000
	audioRate  = 16000 // hlcamd's microphone
	// defaultFrame is a frame's duration when none can be measured (30 fps).
	defaultFrame = videoClock / 30
)

type accessUnit struct {
	pts  int64
	nals [][]byte
	idr  bool
}

type audioUnit struct {
	pts int64 // 90 kHz
	au  []byte
}

// muxer turns the camera's samples into the recording's fMP4.
type muxer struct {
	fragment  int64          // target fragment length, 90 kHz
	aac       *aacenc.Stream // encodes the PCM; nil: none comes in
	withAudio bool           // an audio track

	sps, pps []byte
	cur      *accessUnit // the access unit being assembled
	frag     []*accessUnit
	audio    []audioUnit
	start    int64 // PTS the recording starts at, its first IDR's
	started  bool
	inited   bool // the init segment went out
	seq      uint32
	audioPos int64 // next audio sample's decode time, in audioRate units, -1 before the first
}

// newMuxer returns a muxer cutting fragments of fragmentMs; aac (nil: no
// audio track) encodes the microphone's PCM.
func newMuxer(fragmentMs int, aac *aacenc.Stream) *muxer {
	if fragmentMs <= 0 {
		fragmentMs = 4000
	}
	return &muxer{fragment: int64(fragmentMs) * videoClock / 1000, aac: aac, withAudio: aac != nil, audioPos: -1}
}

// write takes the next sample and returns what it completed, in order:
// the init segment before the first fragment, then fragments.
func (m *muxer) write(s camera.Sample) ([][]byte, error) {
	if !s.IsVideo {
		return nil, m.writePCM(s)
	}
	if m.cur != nil && m.cur.pts == s.PTS {
		m.addNAL(s.Data)
		return nil, nil
	}
	var out [][]byte
	if m.cur != nil {
		var err error
		if out, err = m.push(m.cur); err != nil {
			return nil, err
		}
	}
	m.cur = &accessUnit{pts: s.PTS}
	m.addNAL(s.Data)
	return out, nil
}

// addNAL adds a NAL to the current access unit. The parameter sets go to
// the init segment's avcC rather than in band, which avc1 doesn't need.
func (m *muxer) addNAL(nal []byte) {
	switch nalType(nal) {
	case nalSPS:
		m.sps = nal
	case nalPPS:
		m.pps = nal
	case nalAUD:
	case nalIDR:
		m.cur.idr = true
		m.cur.nals = append(m.cur.nals, nal)
	default:
		m.cur.nals = append(m.cur.nals, nal)
	}
}

func (m *muxer) writePCM(s camera.Sample) error {
	if m.aac == nil {
		return nil
	}
	return m.aac.Write(s.Data, s.PTS, func(au []byte, pts int64) {
		m.audio = append(m.audio, audioUnit{pts: pts, au: append([]byte(nil), au...)})
	})
}

// push files a complete access unit. An IDR closes the fragment before it
// once that one is long enough.
func (m *muxer) push(au *accessUnit) ([][]byte, error) {
	if len(au.nals) == 0 {
		return nil, nil
	}
	if !m.started {
		if !au.idr || m.sps == nil || m.pps == nil {
			return nil, nil // a recording starts on an IDR
		}
		m.started, m.start = true, au.pts
	}
	var out [][]byte
	if au.idr && len(m.frag) > 0 && au.pts-m.frag[0].pts >= m.fragment {
		var err error
		if out, err = m.cut(au.pts); err != nil {
			return nil, err
		}
	}
	m.frag = append(m.frag, au)
	return out, nil
}

// flush cuts what is left, the recording's end.
func (m *muxer) flush() ([][]byte, error) {
	var out [][]byte
	if m.cur != nil {
		var err error
		if out, err = m.push(m.cur); err != nil {
			return nil, err
		}
		m.cur = nil
	}
	if len(m.frag) == 0 {
		return out, nil
	}
	last := m.frag[len(m.frag)-1]
	rest, err := m.cut(last.pts + m.lastDuration())
	return append(out, rest...), err
}

// lastDuration guesses the last access unit's duration from the one
// before it.
func (m *muxer) lastDuration() int64 {
	if n := len(m.frag); n >= 2 {
		if d := m.frag[n-1].pts - m.frag[n-2].pts; d > 0 {
			return d
		}
	}
	return defaultFrame
}

// cut marshals the pending access units, which end at end, with the audio
// before end, as one fragment; the init segment comes first if it has not
// gone out yet.
func (m *muxer) cut(end int64) ([][]byte, error) {
	var out [][]byte
	if !m.inited {
		init, err := m.init()
		if err != nil {
			return nil, err
		}
		out = append(out, init)
		m.inited = true
	}
	video, err := m.videoTrack(end)
	if err != nil {
		return nil, err
	}
	m.seq++
	part := fmp4.Part{SequenceNumber: m.seq, Tracks: []*fmp4.PartTrack{video}}
	if audio := m.audioTrack(end); audio != nil {
		part.Tracks = append(part.Tracks, audio)
	}
	var buf seekablebuffer.Buffer
	if err := part.Marshal(&buf); err != nil {
		return nil, err
	}
	m.frag = m.frag[:0:0]
	return append(out, buf.Bytes()), nil
}

func (m *muxer) init() ([]byte, error) {
	if m.sps == nil || m.pps == nil {
		return nil, errors.New("hksv: no SPS/PPS for the init segment")
	}
	init := fmp4.Init{Tracks: []*fmp4.InitTrack{{
		ID: videoTrack, TimeScale: videoClock,
		Codec: &codecs.H264{SPS: m.sps, PPS: m.pps},
	}}}
	if m.withAudio {
		init.Tracks = append(init.Tracks, &fmp4.InitTrack{
			ID: audioTrack, TimeScale: audioRate,
			Codec: &codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{
				Type: mpeg4audio.ObjectTypeAACLC, SampleRate: audioRate, ChannelConfig: 1,
			}},
		})
	}
	var buf seekablebuffer.Buffer
	if err := init.Marshal(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// videoTrack lays the pending access units out: each lasts until the next
// one's PTS, the last until end.
func (m *muxer) videoTrack(end int64) (*fmp4.PartTrack, error) {
	t := &fmp4.PartTrack{ID: videoTrack, BaseTime: uint64(m.frag[0].pts - m.start)}
	for i, au := range m.frag {
		next := end
		if i+1 < len(m.frag) {
			next = m.frag[i+1].pts
		}
		d := next - au.pts
		if d <= 0 {
			d = defaultFrame // a stuck clock must not make a zero-length frame
		}
		payload, err := h264.AVCC(au.nals).Marshal()
		if err != nil {
			return nil, err
		}
		t.Samples = append(t.Samples, &fmp4.Sample{
			Duration:        uint32(d),
			IsNonSyncSample: !au.idr,
			Payload:         payload,
		})
	}
	return t, nil
}

// audioTrack takes the encoded audio before end. Audio from before the
// recording's start is dropped; after the first unit, the timeline counts
// frames, so that a jittery clock never leaves holes or overlaps.
func (m *muxer) audioTrack(end int64) *fmp4.PartTrack {
	if !m.withAudio {
		return nil
	}
	n := 0
	for n < len(m.audio) && m.audio[n].pts < end {
		n++
	}
	units := m.audio[:n]
	m.audio = append([]audioUnit(nil), m.audio[n:]...)
	for len(units) > 0 && m.audioPos < 0 && units[0].pts < m.start {
		units = units[1:]
	}
	if len(units) == 0 {
		return nil
	}
	if m.audioPos < 0 {
		m.audioPos = (units[0].pts - m.start) * audioRate / videoClock
	}
	frame := int64(aacenc.LC.FrameSamples())
	t := &fmp4.PartTrack{ID: audioTrack, BaseTime: uint64(m.audioPos)}
	for _, u := range units {
		t.Samples = append(t.Samples, &fmp4.Sample{Duration: uint32(frame), Payload: u.au})
	}
	m.audioPos += frame * int64(len(units))
	return t
}
