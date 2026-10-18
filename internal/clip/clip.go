// Package clip records a few seconds of the camera's H.264 stream into an
// in-memory progressive MP4, ready to be sent to Telegram.
package clip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"

	"github.com/caligone/openqiara/internal/camera"
)

// Clip is a recorded video: a progressive MP4 (moov before mdat), one H.264 track.
type Clip struct {
	MP4      []byte
	Width    int
	Height   int
	Duration time.Duration
}

// MaxBytes caps a clip's size: recording stops early when reached. A var
// for tests. The camera has about 58 MB free and peak memory is about
// 2x MaxBytes (access units plus the muxed output); a nominal 15 s clip
// is only about 2 MB, so 8 MiB leaves ample headroom.
var MaxBytes = 8 << 20

const (
	timeScale    = 90000
	defaultFrame = 3000 // 30 fps, when a clip has a single frame
)

// accessUnit is the NAL units sharing one PTS, in AVCC form (4-byte
// length prefix), without parameter sets and delimiters.
type accessUnit struct {
	pts  int64
	data []byte
	idr  bool
}

type recorder struct {
	sps, pps []byte // latest seen, even before the clip starts
	cur      accessUnit
	hasCur   bool

	started  bool
	codecSPS []byte
	codecPPS []byte
	first    int64
	aus      []accessUnit
	size     int
	done     bool
}

// add feeds one sample; it reports whether recording is over.
func (r *recorder) add(s camera.Sample, limit int64) bool {
	if !s.IsVideo || len(s.Data) == 0 {
		return false
	}
	if r.hasCur && s.PTS != r.cur.pts {
		r.flush(limit)
		if r.done {
			return true
		}
	}
	if !r.hasCur {
		r.cur = accessUnit{pts: s.PTS}
		r.hasCur = true
	}
	switch s.Data[0] & 0x1f {
	case 7:
		r.sps = append(r.sps[:0], s.Data...)
	case 8:
		r.pps = append(r.pps[:0], s.Data...)
	case 9: // access unit delimiter
	default:
		if s.Data[0]&0x1f == 5 {
			r.cur.idr = true
		}
		n := len(s.Data)
		r.cur.data = append(r.cur.data, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		r.cur.data = append(r.cur.data, s.Data...)
	}
	return false
}

// flush closes the pending access unit and keeps it if it belongs to the clip.
func (r *recorder) flush(limit int64) {
	if !r.hasCur {
		return
	}
	au := r.cur
	r.cur, r.hasCur = accessUnit{}, false
	if len(au.data) == 0 {
		return
	}
	if !r.started {
		if !au.idr || r.sps == nil || r.pps == nil {
			return
		}
		r.started = true
		r.first = au.pts
		r.codecSPS = append([]byte(nil), r.sps...)
		r.codecPPS = append([]byte(nil), r.pps...)
	}
	if au.pts-r.first >= limit || (len(r.aus) > 0 && r.size+len(au.data) > MaxBytes) {
		r.done = true
		return
	}
	r.aus = append(r.aus, au)
	r.size += len(au.data)
}

// Record reads samples from src for d of video, starting at the first IDR
// access unit preceded by SPS and PPS (earlier samples are skipped), and
// returns the clip. It stops at d of PTS, at MaxBytes, when src closes or
// when ctx ends; it returns an error only if no IDR was received at all.
// The caller must pass a ctx with a deadline: a stalled source is only
// ended by ctx.
func Record(ctx context.Context, src <-chan camera.Sample, d time.Duration) (Clip, error) {
	limit := int64(d) * timeScale / int64(time.Second)
	var r recorder
loop:
	for !r.done {
		select {
		case <-ctx.Done():
			break loop
		case s, ok := <-src:
			if !ok {
				break loop
			}
			if r.add(s, limit) {
				break loop
			}
		}
	}
	r.flush(limit)
	if len(r.aus) == 0 {
		return Clip{}, errors.New("clip: no IDR frame received")
	}
	return r.build()
}

func (r *recorder) build() (Clip, error) {
	var sps h264.SPS
	if err := sps.Unmarshal(r.codecSPS); err != nil {
		return Clip{}, fmt.Errorf("clip: parsing SPS: %w", err)
	}
	width, height := sps.Width(), sps.Height()

	samples := make([]*pmp4.Sample, len(r.aus))
	var total int64
	last := uint32(defaultFrame)
	for i, au := range r.aus {
		dur := last
		if i+1 < len(r.aus) {
			dur = uint32(r.aus[i+1].pts - au.pts)
			last = dur
		}
		data := au.data
		samples[i] = &pmp4.Sample{
			Duration:        dur,
			IsNonSyncSample: !au.idr,
			PayloadSize:     uint32(len(data)),
			GetPayload:      func() ([]byte, error) { return data, nil },
		}
		total += int64(dur)
	}

	p := pmp4.Presentation{Tracks: []*pmp4.Track{{
		ID:        1,
		TimeScale: timeScale,
		Codec:     &codecs.H264{SPS: r.codecSPS, PPS: r.codecPPS},
		Samples:   samples,
	}}}
	var buf bytes.Buffer
	buf.Grow(r.size + 4096)
	if err := p.Marshal(&buf); err != nil {
		return Clip{}, fmt.Errorf("clip: muxing: %w", err)
	}
	return Clip{
		MP4:      buf.Bytes(),
		Width:    width,
		Height:   height,
		Duration: time.Duration(total) * time.Second / timeScale,
	}, nil
}
