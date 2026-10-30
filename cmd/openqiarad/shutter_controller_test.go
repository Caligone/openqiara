package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
)

// fakeShutter records the shutter's moves.
type fakeShutter struct {
	calls []string
	err   error
}

func (f *fakeShutter) set(_ context.Context, open bool) error {
	call := "close"
	if open {
		call = "open"
	}
	f.calls = append(f.calls, call)
	return f.err
}

func newTestShutterController(shutter *fakeShutter, enabled *bool) *shutterController {
	c := newShutterController(context.Background(), shutter.set, func() bool { return *enabled },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.synchronous = true
	return c
}

// TestShutterFollowsAlarm: the shutter opens when the alarm is set, from
// the exit delay or straight armed, in every mode, and closes when it is
// disarmed; the states in between move nothing.
func TestShutterFollowsAlarm(t *testing.T) {
	shutter := &fakeShutter{}
	enabled := true
	c := newTestShutterController(shutter, &enabled)

	for _, state := range []string{
		"disarmed", // the start's: moves nothing
		"arming", "armed_away", "pending", "triggered", "disarmed", "disarmed",
		"armed_night", "armed_home", "disarmed", // armed remotely, no exit delay
		"armed_vacation", "unknown", "disarmed", // Alarmo's
	} {
		c.Handle(state)
	}
	want := []string{"open", "close", "open", "close", "open", "close"}
	if !slices.Equal(shutter.calls, want) {
		t.Errorf("shutter moves = %q, want %q", shutter.calls, want)
	}
}

// TestShutterStartMovesNothing: the state at start, restored or retained,
// leaves the shutter as saved, armed too.
func TestShutterStartMovesNothing(t *testing.T) {
	shutter := &fakeShutter{}
	enabled := true
	c := newTestShutterController(shutter, &enabled)

	c.Handle("unknown")
	c.Handle("armed_away")
	c.Handle("triggered")
	if len(shutter.calls) != 0 {
		t.Fatalf("shutter moved at start: %q", shutter.calls)
	}
	c.Handle("disarmed")
	if !slices.Equal(shutter.calls, []string{"close"}) {
		t.Errorf("shutter moves = %q, want close", shutter.calls)
	}
}

// TestShutterOptionOff: off, the alarm moves nothing; turned on, the
// next change moves the shutter, without a restart.
func TestShutterOptionOff(t *testing.T) {
	shutter := &fakeShutter{}
	enabled := false
	c := newTestShutterController(shutter, &enabled)

	for _, state := range []string{"disarmed", "arming", "armed_away", "disarmed", "armed_night"} {
		c.Handle(state)
	}
	if len(shutter.calls) != 0 {
		t.Fatalf("shutter moved with the option off: %q", shutter.calls)
	}
	enabled = true
	c.Handle("armed_night")
	c.Handle("disarmed")
	if !slices.Equal(shutter.calls, []string{"close"}) {
		t.Errorf("shutter moves = %q, want close", shutter.calls)
	}
}

// TestShutterFailureNotRetried: a move that fails is not tried again
// until the alarm next changes.
func TestShutterFailureNotRetried(t *testing.T) {
	shutter := &fakeShutter{err: errors.New("radio down")}
	enabled := true
	c := newTestShutterController(shutter, &enabled)

	c.Handle("disarmed")
	c.Handle("arming")
	c.Handle("armed_away")
	shutter.err = nil
	c.Handle("disarmed")
	if want := []string{"open", "close"}; !slices.Equal(shutter.calls, want) {
		t.Errorf("shutter moves = %q, want %q", shutter.calls, want)
	}
}
