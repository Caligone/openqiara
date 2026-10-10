package mediahub

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/camera"
)

type countingResumer struct{ calls atomic.Int32 }

func (r *countingResumer) ResumeIfStale(context.Context) bool {
	r.calls.Add(1)
	return true
}

// A source that sends n samples, then nothing: hlcamd frozen.
func freezingSource(n int) Source {
	return func(ctx context.Context) (<-chan camera.Sample, error) {
		out := make(chan camera.Sample)
		go func() {
			defer close(out)
			for i := range n {
				select {
				case out <- camera.Sample{IsVideo: true, PTS: int64(i)}:
				case <-ctx.Done():
					return
				}
			}
			<-ctx.Done()
		}()
		return out, nil
	}
}

func TestHubWakesAFrozenStream(t *testing.T) {
	res := &countingResumer{}
	h := New("test", freezingSource(3), res, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.staleCheck = 20 * time.Millisecond
	if !h.LastSample().IsZero() {
		t.Fatal("a sample before any subscriber")
	}
	sub := h.Subscribe()
	defer sub.Close()
	for range 3 {
		<-sub.Samples()
	}
	if time.Since(h.LastSample()) > time.Second {
		t.Fatalf("LastSample = %v", h.LastSample())
	}
	// Once at start, then on every check while someone watches.
	deadline := time.Now().Add(2 * time.Second)
	for res.calls.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("resumer asked %d times", res.calls.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMergeForwardsBothThenCloses(t *testing.T) {
	a, b := make(chan camera.Sample), make(chan camera.Sample)
	out := merge(context.Background(), a, b)
	go func() {
		a <- camera.Sample{IsVideo: true}
		close(a)
	}()
	go func() {
		b <- camera.Sample{Data: []byte{1}}
		b <- camera.Sample{Data: []byte{2}}
		close(b)
	}()
	video, audio := 0, 0
	for s := range out {
		if s.IsVideo {
			video++
		} else {
			audio++
		}
	}
	if video != 1 || audio != 2 {
		t.Fatalf("%d video, %d audio, want 1 and 2", video, audio)
	}
}

// A source that fails is started again while someone listens.
func TestHubRestartsAFailedSource(t *testing.T) {
	var starts atomic.Int32
	src := func(ctx context.Context) (<-chan camera.Sample, error) {
		starts.Add(1)
		out := make(chan camera.Sample, 1)
		out <- camera.Sample{IsVideo: true}
		close(out) // the source dies right away
		return out, nil
	}
	h := New("test", src, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sub := h.Subscribe()
	defer sub.Close()
	for range 2 {
		select {
		case <-sub.Samples():
		case <-time.After(3 * restartDelay):
			t.Fatalf("source started %d times", starts.Load())
		}
	}
}
