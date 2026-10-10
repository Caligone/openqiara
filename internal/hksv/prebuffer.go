package hksv

import "github.com/caligone/openqiara/internal/camera"

// A recording starts before its trigger: iOS asks for a prebuffer (4 s)
// and the camera must have it ready when the motion comes. The prebuffer
// keeps the camera's samples, as they come, from an IDR on: a recording
// can only start on one.

// maxPrebufferBytes bounds the prebuffer whatever the stream does: 4 s at
// 2 Mbit/s is 1 MB, so this only trips on a stream gone wrong (no IDR,
// stuck PTS).
const maxPrebufferBytes = 8 << 20

// NAL unit types the recording cares about.
const (
	nalIDR = 5
	nalSPS = 7
	nalPPS = 8
	nalAUD = 9
)

func nalType(nal []byte) byte {
	if len(nal) == 0 {
		return 0
	}
	return nal[0] & 0x1f
}

// prebuffer keeps the shortest run of whole GOPs covering keep (90 kHz),
// with the audio that came along.
type prebuffer struct {
	keep    int64
	samples []camera.Sample
	starts  []int // index in samples of each GOP's first sample
	size    int
	last    int64 // PTS of the newest video sample
}

// gopStart tells whether s opens a GOP: the first NAL of an access unit
// holding an IDR, the SPS hlcamd sends before each one, or the IDR itself
// when no SPS came at that PTS.
func (p *prebuffer) gopStart(s camera.Sample) bool {
	if !s.IsVideo {
		return false
	}
	t := nalType(s.Data)
	if t != nalSPS && t != nalIDR {
		return false
	}
	if len(p.starts) == 0 {
		return true
	}
	return p.samples[p.starts[len(p.starts)-1]].PTS != s.PTS
}

func (p *prebuffer) add(s camera.Sample) {
	if p.gopStart(s) {
		p.starts = append(p.starts, len(p.samples))
	}
	if len(p.starts) == 0 {
		return // nothing to start a recording from yet
	}
	p.samples = append(p.samples, s)
	p.size += len(s.Data)
	if s.IsVideo {
		p.last = s.PTS
	}
	p.trim()
}

// trim drops the oldest GOP while the next one still covers keep, or the
// buffer is too big.
func (p *prebuffer) trim() {
	for len(p.starts) >= 2 {
		second := p.samples[p.starts[1]].PTS
		if p.last-second < p.keep && p.size <= maxPrebufferBytes {
			return
		}
		p.drop(p.starts[1])
	}
	if p.size > maxPrebufferBytes {
		p.reset()
	}
}

// drop forgets the first n samples.
func (p *prebuffer) drop(n int) {
	for _, s := range p.samples[:n] {
		p.size -= len(s.Data)
	}
	clear(p.samples[:n]) // let the dropped data go
	p.samples = p.samples[n:]
	for i := range p.starts {
		p.starts[i] -= n
	}
	p.starts = p.starts[1:]
}

func (p *prebuffer) reset() {
	*p = prebuffer{keep: p.keep}
}

// snapshot returns what a recording starts with: the buffer from its
// oldest IDR.
func (p *prebuffer) snapshot() []camera.Sample {
	return append([]camera.Sample(nil), p.samples...)
}
