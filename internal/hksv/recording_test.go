package hksv

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"

	"github.com/caligone/openqiara/internal/camera"
)

// A 1920x1080 Constrained Baseline SPS and its PPS, as hlcamd sends them.
var (
	testSPS = []byte{0x67, 0x42, 0x40, 0x28, 0xa6, 0x80, 0x78, 0x02, 0x27, 0xe5, 0x84, 0x00, 0x00, 0x0f, 0xa4, 0x00, 0x03, 0xaa, 0x70, 0x10}
	testPPS = []byte{0x68, 0xca, 0x8f, 0x20}
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// frame returns frame f of hlcamd's stream (30 fps, an IDR with its
// parameter sets every 15 frames) and, every other frame, the microphone's
// PCM, as the hub delivers them.
func frame(f int64) []camera.Sample {
	pts := f * 3000
	nals := [][]byte{{0x41, 0x9a, byte(f)}}
	if f%15 == 0 {
		nals = [][]byte{testSPS, testPPS, {0x65, 0x88, 0x84, byte(f)}}
	}
	var out []camera.Sample
	for _, n := range nals {
		out = append(out, camera.Sample{IsVideo: true, PTS: pts, Data: n})
	}
	if f%2 == 0 {
		out = append(out, camera.Sample{PTS: pts, Data: make([]byte, 2048)})
	}
	return out
}

// fakeCamera sends frames as fast as every period.
func fakeCamera(period time.Duration) func(ctx context.Context) (<-chan camera.Sample, error) {
	return func(ctx context.Context) (<-chan camera.Sample, error) {
		out := make(chan camera.Sample)
		go func() {
			defer close(out)
			tick := time.NewTicker(period)
			defer tick.Stop()
			for f := int64(0); ; f++ {
				for _, s := range frame(f) {
					select {
					case out <- s:
					case <-ctx.Done():
						return
					}
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, nil
	}
}

func TestPrebufferKeepsWholeGOPs(t *testing.T) {
	p := prebuffer{keep: 4 * videoClock}
	for _, s := range frame(1) { // before any IDR: nothing to start from
		p.add(s)
	}
	if len(p.samples) != 0 {
		t.Fatalf("%d samples before the first IDR", len(p.samples))
	}
	for f := int64(15); f < 15+30*10; f++ {
		for _, s := range frame(f) {
			p.add(s)
		}
	}
	snap := p.snapshot()
	if nalType(snap[0].Data) != nalSPS {
		t.Fatalf("starts with NAL type %d", nalType(snap[0].Data))
	}
	span := p.last - snap[0].PTS
	if span < 4*videoClock || span >= 4*videoClock+15*3000 {
		t.Fatalf("keeps %d ms", span/90)
	}

	// A stream gone wrong (no IDR any more) is bounded.
	big := make([]byte, 1<<20)
	for i := range 20 {
		p.add(camera.Sample{IsVideo: true, PTS: p.last + int64(i+1)*3000, Data: append([]byte{0x41}, big...)})
	}
	if p.size > maxPrebufferBytes {
		t.Fatalf("%d bytes kept", p.size)
	}
}

// parse checks an init segment and fragments with mediacommon, returning
// the fragments.
func parse(t *testing.T, init []byte, frags ...[]byte) (fmp4.Init, fmp4.Parts) {
	t.Helper()
	var in fmp4.Init
	if err := in.Unmarshal(bytes.NewReader(init)); err != nil {
		t.Fatalf("init segment: %v", err)
	}
	var parts fmp4.Parts
	if err := parts.Unmarshal(bytes.Join(frags, nil)); err != nil {
		t.Fatalf("fragments: %v", err)
	}
	return in, parts
}

func TestMuxerFragments(t *testing.T) {
	for _, audio := range []bool{false, true} {
		m := newMuxer(4000, nil)
		m.withAudio = audio // the encoder may be missing here: AUs come in by hand
		var out [][]byte
		for f := int64(5); f < 5+30*8; f++ { // starts mid-GOP, 8 s
			if audio && f%2 == 0 {
				m.audio = append(m.audio, audioUnit{pts: f * 3000, au: []byte{0x21, byte(f)}})
			}
			for _, s := range frame(f) {
				o, err := m.write(s)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, o...)
			}
		}
		rest, err := m.flush()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rest...)
		if len(out) != 3 {
			t.Fatalf("audio %v: %d outputs, want init and two fragments", audio, len(out))
		}
		in, parts := parse(t, out[0], out[1:]...)
		if want := map[bool]int{false: 1, true: 2}[audio]; len(in.Tracks) != want {
			t.Fatalf("%d tracks, want %d", len(in.Tracks), want)
		}
		if len(parts) != 2 {
			t.Fatalf("%d fragments", len(parts))
		}
		var base uint64
		for i, p := range parts {
			if p.SequenceNumber != uint32(i+1) {
				t.Fatalf("fragment %d: sequence number %d", i, p.SequenceNumber)
			}
			v := p.Tracks[0]
			if v.ID != videoTrack || v.Samples[0].IsNonSyncSample {
				t.Fatalf("fragment %d does not start with an IDR", i)
			}
			if v.BaseTime != base {
				t.Fatalf("fragment %d: base time %d, want %d", i, v.BaseTime, base)
			}
			for _, s := range v.Samples {
				base += uint64(s.Duration)
			}
			if audio && (len(p.Tracks) != 2 || p.Tracks[1].ID != audioTrack) {
				t.Fatalf("fragment %d without audio", i)
			}
		}
		// It starts at frame 15, the first IDR: 4 s, then the rest.
		if d := parts[1].Tracks[0].BaseTime; d != 4*videoClock {
			t.Fatalf("first fragment lasts %d ms", d/90)
		}
		// Audio from before the first IDR is dropped; the first kept
		// comes a frame after it.
		if audio && parts[0].Tracks[1].BaseTime != 3000*audioRate/videoClock {
			t.Fatalf("audio starts at %d", parts[0].Tracks[1].BaseTime)
		}
	}
}

func TestRecorderStreamsPrebufferThenLive(t *testing.T) {
	hub := newHub(t)
	r := NewRecorder(1000, quiet)
	if err := r.Record(context.Background(), Options{}, nil); err != errNotRecording {
		t.Fatalf("recording while off: %v", err)
	}
	r.SetHub(hub)
	r.SetActive(true)
	waitPrebuffer(t, r)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got [][]byte
	err := r.Record(ctx, Options{FragmentMs: 1000}, func(b []byte, last bool) error {
		got = append(got, b)
		if len(got) == 3 {
			cancel()
		}
		return nil
	})
	if err != context.Canceled {
		t.Fatal(err)
	}
	_, parts := parse(t, got[0], got[1:]...)
	if len(parts) != 2 {
		t.Fatalf("%d fragments", len(parts))
	}

	// Off: the stream ends, the camera is let go.
	r.SetActive(false)
	if r.stop != nil || len(r.buf.samples) != 0 {
		t.Fatal("still prebuffering")
	}
}

func waitPrebuffer(t *testing.T, r *Recorder) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		n := len(r.buf.starts)
		r.mu.Unlock()
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no prebuffer")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRecordingEndsWithTheStream(t *testing.T) {
	r := NewRecorder(1000, quiet)
	r.SetHub(newHub(t))
	r.SetActive(true)
	waitPrebuffer(t, r)

	var got [][]byte
	last := false
	err := r.Record(context.Background(), Options{FragmentMs: 1000}, func(b []byte, l bool) error {
		if last {
			t.Fatal("data after the last fragment")
		}
		got, last = append(got, b), l
		if len(got) == 2 {
			go r.SetActive(false) // recording turned off in Home
		}
		return nil
	})
	if err != nil || !last {
		t.Fatalf("err %v, last %v", err, last)
	}
	parse(t, got[0], got[1:]...)
}
