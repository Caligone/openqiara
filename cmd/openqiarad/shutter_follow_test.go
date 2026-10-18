package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type shutterRec struct {
	mu    sync.Mutex
	calls []bool
	ch    chan bool
}

func newShutterRec() *shutterRec { return &shutterRec{ch: make(chan bool, 100)} }

func (r *shutterRec) set(_ context.Context, open bool) error {
	r.mu.Lock()
	r.calls = append(r.calls, open)
	r.mu.Unlock()
	r.ch <- open
	return nil
}

func (r *shutterRec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func expectCall(t *testing.T, ch <-chan bool, want bool) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("set(open=%v), want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for set(open=%v)", want)
	}
}

func TestShutterFollowerTransitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newShutterRec()
	f := newShutterFollower(ctx, rec.set, func() bool { return true }, quietLogger())

	f.AlarmState("arming")
	expectCall(t, rec.ch, true)
	f.AlarmState("armed_away")
	f.AlarmState("disarmed")
	expectCall(t, rec.ch, false)
	f.AlarmState("disarmed")
	// Barrier: a further distinct request must be the next call, proving
	// nothing else was queued before it.
	f.AlarmState("triggered")
	expectCall(t, rec.ch, true)
	if n := rec.count(); n != 3 {
		t.Fatalf("calls = %d, want 3", n)
	}
}

func TestShutterFollowerDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newShutterRec()
	var mu sync.Mutex
	on := false
	f := newShutterFollower(ctx, rec.set, func() bool { mu.Lock(); defer mu.Unlock(); return on }, quietLogger())

	f.AlarmState("armed_away")
	f.AlarmState("disarmed")
	// Enable afterwards: the disabled calls must not have been remembered.
	mu.Lock()
	on = true
	mu.Unlock()
	f.AlarmState("disarmed")
	expectCall(t, rec.ch, false)
	if n := rec.count(); n != 1 {
		t.Fatalf("calls = %d, want 1 (none while disabled)", n)
	}
}

func TestShutterFollowerNeverBlocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	set := func(ctx context.Context, open bool) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}
	f := newShutterFollower(ctx, set, func() bool { return true }, quietLogger())
	f.AlarmState("armed_away")
	<-entered

	start := time.Now()
	for i := 0; i < 100; i++ {
		if i%2 == 0 {
			f.AlarmState("disarmed")
		} else {
			f.AlarmState("armed_away")
		}
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("100 AlarmState calls took %v", d)
	}
	close(release)
}

func TestShutterFollowerLatestWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	rec := newShutterRec()
	first := true
	var mu sync.Mutex
	set := func(ctx context.Context, open bool) error {
		mu.Lock()
		blocking := first
		first = false
		mu.Unlock()
		if blocking {
			entered <- struct{}{}
			<-release
		}
		return rec.set(ctx, open)
	}
	f := newShutterFollower(ctx, set, func() bool { return true }, quietLogger())

	f.AlarmState("armed_away")
	<-entered
	f.AlarmState("disarmed")
	f.AlarmState("armed_away")
	f.AlarmState("disarmed")
	close(release)

	expectCall(t, rec.ch, true)  // the blocked open completes
	expectCall(t, rec.ch, false) // the latest wanted position
	if n := rec.count(); n != 2 {
		t.Fatalf("calls = %d, want at most 2", n)
	}
}

func TestShutterFollowerRetryAfterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := make(chan bool, 10)
	var mu sync.Mutex
	fail := true
	set := func(_ context.Context, open bool) error {
		mu.Lock()
		defer mu.Unlock()
		attempts <- open
		if fail {
			fail = false
			return errors.New("boom")
		}
		return nil
	}
	f := newShutterFollower(ctx, set, func() bool { return true }, quietLogger())

	f.AlarmState("armed_away")
	expectCall(t, attempts, true)
	// The failure is recorded before the worker takes its next request, so
	// a new identical request is retried; poll via the retry itself.
	deadline := time.After(5 * time.Second)
	for {
		f.AlarmState("armed_away")
		select {
		case got := <-attempts:
			if !got {
				t.Fatal("retry should open")
			}
			return
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatal("no retry after failure")
		}
	}
}
