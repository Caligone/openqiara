package clip

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"

	"github.com/caligone/openqiara/internal/camera"
)

// A 1920x1080 Constrained Baseline SPS and its PPS, as hlcamd sends them.
var (
	testSPS = []byte{0x67, 0x42, 0x40, 0x28, 0xa6, 0x80, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x0f, 0xa4, 0x00, 0x03, 0xaa, 0x70, 0x10}
	testPPS = []byte{0x68, 0xca, 0x8f, 0x20}
)

const frameTicks = 3000 // 30 fps

// frame returns frame f: an IDR with SPS/PPS every 15 frames, else a P frame.
func frame(f int64) []camera.Sample {
	pts := f * frameTicks
	var nals [][]byte
	if f%15 == 0 {
		nals = [][]byte{testSPS, testPPS, {0x65, 0x88, 0x84, byte(f), 1, 2, 3, 4}}
	} else {
		nals = [][]byte{{0x41, 0x9a, byte(f), 1, 2, 3, 4}}
	}
	out := make([]camera.Sample, len(nals))
	for i, n := range nals {
		out[i] = camera.Sample{IsVideo: true, PTS: pts, Data: n}
	}
	return out
}

// feed returns a closed channel holding frames [from, to).
func feed(from, to int64) <-chan camera.Sample {
	ch := make(chan camera.Sample, 8192)
	for f := from; f < to; f++ {
		for _, s := range frame(f) {
			ch <- s
		}
	}
	close(ch)
	return ch
}

func parse(t *testing.T, mp4 []byte) *pmp4.Presentation {
	t.Helper()
	var p pmp4.Presentation
	if err := p.Unmarshal(bytes.NewReader(mp4)); err != nil {
		t.Fatalf("invalid MP4: %v", err)
	}
	if len(p.Tracks) != 1 {
		t.Fatalf("tracks = %d", len(p.Tracks))
	}
	return &p
}

func TestRecordStartsAtIDR(t *testing.T) {
	// frames 3..14 are P frames before the first IDR (15).
	c, err := Record(context.Background(), feed(3, 60), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, c.MP4)
	samples := p.Tracks[0].Samples
	if len(samples) != 45 {
		t.Fatalf("samples = %d, want 45", len(samples))
	}
	if samples[0].IsNonSyncSample {
		t.Fatal("first sample is not a sync sample")
	}
	pl, err := samples[0].GetPayload()
	if err != nil {
		t.Fatal(err)
	}
	if pl[4]&0x1f != 5 {
		t.Fatalf("first NAL type = %d, want IDR", pl[4]&0x1f)
	}
	if pl[7] != 15 { // our marker byte: frame 15
		t.Fatalf("first frame = %d, want 15", pl[7])
	}
}

func TestRecordStopsAtDuration(t *testing.T) {
	c, err := Record(context.Background(), feed(0, 900), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := parse(t, c.MP4)
	n := len(p.Tracks[0].Samples)
	if n < 449 || n > 451 {
		t.Fatalf("samples = %d, want 450 +-1", n)
	}
	want := time.Duration(n) * time.Second / 30
	if diff := c.Duration - want; diff < -time.Millisecond || diff > time.Millisecond {
		t.Fatalf("Duration = %v, want %v", c.Duration, want)
	}
}

func TestRecordStopsAtMaxBytes(t *testing.T) {
	old := MaxBytes
	MaxBytes = 2000
	defer func() { MaxBytes = old }()
	c, err := Record(context.Background(), feed(0, 900), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.MP4) > 2000+4096 {
		t.Fatalf("mp4 = %d bytes, not bounded", len(c.MP4))
	}
	n := len(parse(t, c.MP4).Tracks[0].Samples)
	if n == 0 || n >= 450 {
		t.Fatalf("samples = %d, want early stop", n)
	}
}

func TestRecordMoovFirst(t *testing.T) {
	c, err := Record(context.Background(), feed(0, 60), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for off := 0; off+8 <= len(c.MP4); {
		size := int(binary.BigEndian.Uint32(c.MP4[off:]))
		order = append(order, string(c.MP4[off+4:off+8]))
		if size < 8 {
			t.Fatal("bad box size")
		}
		off += size
	}
	moov, mdat := -1, -1
	for i, b := range order {
		if b == "moov" {
			moov = i
		}
		if b == "mdat" {
			mdat = i
		}
	}
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Fatalf("box order = %v", order)
	}
	if c.Width != 1920 || c.Height != 1080 {
		t.Fatalf("size = %dx%d", c.Width, c.Height)
	}
}

func TestRecordNoIDR(t *testing.T) {
	ch := make(chan camera.Sample, 10)
	for f := int64(1); f < 10; f++ {
		ch <- camera.Sample{IsVideo: true, PTS: f * frameTicks, Data: []byte{0x41, 0x9a, 1}}
	}
	close(ch)
	if _, err := Record(context.Background(), ch, time.Second); err == nil {
		t.Fatal("expected an error")
	}
}

func TestRecordCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan camera.Sample, 100)
	for f := int64(0); f < 20; f++ {
		for _, s := range frame(f) {
			ch <- s
		}
	}
	done := make(chan struct{})
	var c Clip
	var err error
	go func() {
		c, err = Record(ctx, ch, time.Minute)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record did not return")
	}
	if err != nil {
		t.Fatal(err)
	}
	if n := len(parse(t, c.MP4).Tracks[0].Samples); n != 20 {
		t.Fatalf("samples = %d, want 20", n)
	}
}
