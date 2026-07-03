package mediahub

import (
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

// newTestHub returns a hub with no pipeline running (subs map ready) so we
// can exercise broadcast/subscribe/unsubscribe without touching disk.
func newTestHub() *Hub {
	return New("/nonexistent.m3u8", nil, nil)
}

func TestBroadcastFanout(t *testing.T) {
	h := newTestHub()
	// Attach two subscribers directly (bypass Subscribe so no pipeline
	// goroutine spins up on this fake path).
	s1 := &subscription{ch: make(chan camera.Sample, subBuffer), hub: h}
	s2 := &subscription{ch: make(chan camera.Sample, subBuffer), hub: h}
	h.subs[s1] = struct{}{}
	h.subs[s2] = struct{}{}

	want := camera.Sample{IsVideo: true, PTS: 42, Data: []byte{0x65}}
	h.broadcast(want)

	for i, s := range []*subscription{s1, s2} {
		select {
		case got := <-s.ch:
			if got.PTS != want.PTS || got.IsVideo != want.IsVideo {
				t.Fatalf("sub %d: got %+v, want %+v", i, got, want)
			}
		default:
			t.Fatalf("sub %d: expected a sample, channel empty", i)
		}
	}
}

func TestBroadcastLossyDoesNotBlock(t *testing.T) {
	h := newTestHub()
	slow := &subscription{ch: make(chan camera.Sample, subBuffer), hub: h}
	h.subs[slow] = struct{}{}

	// Fill the buffer, then push one more: broadcast must not block and
	// the overflow sample is silently dropped for this subscriber.
	for i := 0; i < subBuffer; i++ {
		h.broadcast(camera.Sample{IsVideo: true, PTS: int64(i)})
	}
	done := make(chan struct{})
	go func() {
		h.broadcast(camera.Sample{IsVideo: true, PTS: 9999}) // must not block
		close(done)
	}()
	<-done // if broadcast blocked, the test hangs and fails via timeout

	if len(slow.ch) != subBuffer {
		t.Fatalf("buffer len = %d, want %d (overflow should be dropped)", len(slow.ch), subBuffer)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	h := newTestHub()
	sub := &subscription{ch: make(chan camera.Sample, subBuffer), hub: h}
	h.subs[sub] = struct{}{}

	h.unsubscribe(sub)

	if _, ok := <-sub.ch; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}
	if len(h.subs) != 0 {
		t.Fatalf("subs not empty after unsubscribe: %d", len(h.subs))
	}
	// Double unsubscribe is a no-op, not a panic.
	h.unsubscribe(sub)
}
