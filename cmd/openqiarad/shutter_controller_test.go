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

func newTestShutterController(shutter *fakeShutter, follows *string) *shutterController {
	c := newShutterController(context.Background(), shutter.set, func() string { return *follows },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.synchronous = true
	return c
}

// step is a state the controller gets, with the mode being armed during
// the exit delay ("" when not known, as from Alarmo).
type step struct{ state, arming string }

func runSteps(c *shutterController, steps []step) {
	for _, s := range steps {
		c.Handle(s.state, s.arming)
	}
}

// TestShutterWant: where each state puts the shutter, for each option.
func TestShutterWant(t *testing.T) {
	const notTold = "-"
	for _, tc := range []struct {
		follows, state, arming string
		want                   string // "open", "close" or notTold
	}{
		{"none", "armed_away", "", notTold},
		{"", "armed_away", "", notTold},

		{"all", "disarmed", "", "close"},
		{"all", "arming", "", "open"},
		{"all", "armed_away", "", "open"},
		{"all", "armed_night", "", "open"},
		{"all", "armed_home", "", "open"},
		{"all", "armed_vacation", "", "open"},
		{"all", "pending", "", "open"},
		{"all", "triggered", "", "open"},
		{"all", "unknown", "", notTold},

		{"away", "disarmed", "", "close"},
		{"away", "arming", "", notTold}, // Alarmo: the mode it arms is not told
		{"away", "arming", "armed_away", "open"},
		{"away", "arming", "armed_night", "close"},
		{"away", "armed_away", "", "open"},
		{"away", "armed_vacation", "", "open"},
		{"away", "armed_night", "", "close"},
		{"away", "armed_home", "", "close"},
		{"away", "armed_custom_bypass", "", "close"},
		{"away", "pending", "", "open"}, // set off: open, whatever the mode
		{"away", "triggered", "", "open"},
		{"away", "unknown", "", notTold},
	} {
		open, ok := shutterWant(tc.follows, tc.state, tc.arming)
		got := notTold
		if ok {
			got = map[bool]string{true: "open", false: "close"}[open]
		}
		if got != tc.want {
			t.Errorf("shutterWant(%q, %q, %q) = %s, want %s", tc.follows, tc.state, tc.arming, got, tc.want)
		}
	}
}

// TestShutterFollowsAll: the shutter opens when the alarm is set, from the
// exit delay or straight armed, in every mode, and closes when it is
// disarmed; the states in between move nothing.
func TestShutterFollowsAll(t *testing.T) {
	shutter := &fakeShutter{}
	follows := "all"
	runSteps(newTestShutterController(shutter, &follows), []step{
		{"disarmed", ""}, // the start's: moves nothing
		{"arming", ""}, {"armed_away", ""}, {"pending", ""}, {"triggered", ""}, {"disarmed", ""}, {"disarmed", ""},
		{"armed_night", ""}, {"armed_home", ""}, {"disarmed", ""}, // armed remotely, no exit delay
		{"armed_vacation", ""}, {"unknown", ""}, {"disarmed", ""}, // Alarmo's
	})
	want := []string{"open", "close", "open", "close", "open", "close"}
	if !slices.Equal(shutter.calls, want) {
		t.Errorf("shutter moves = %q, want %q", shutter.calls, want)
	}
}

// TestShutterFollowsAwayStandalone: with the standalone engine, the shutter
// opens from the exit delay when arming away, stays closed armed for the
// night, opens once the alarm is set off, and follows a switch of mode.
func TestShutterFollowsAwayStandalone(t *testing.T) {
	shutter := &fakeShutter{}
	follows := "away"
	runSteps(newTestShutterController(shutter, &follows), []step{
		{"disarmed", "armed_away"}, // the start's
		{"arming", "armed_away"}, {"armed_away", "armed_away"}, {"disarmed", "armed_away"},
		{"arming", "armed_night"}, {"armed_night", "armed_night"}, // stays closed
		{"pending", "armed_night"}, {"triggered", "armed_night"}, {"disarmed", "armed_night"},
		{"armed_away", "armed_away"},   // armed remotely
		{"armed_night", "armed_night"}, // mode switched
		{"armed_away", "armed_away"},
		{"disarmed", "armed_away"},
	})
	want := []string{"open", "close", "open", "close", "open", "close", "open", "close"}
	if !slices.Equal(shutter.calls, want) {
		t.Errorf("shutter moves = %q, want %q", shutter.calls, want)
	}
}

// TestShutterFollowsAwayAlarmo: with Alarmo, which does not tell the mode
// it arms, the shutter waits for the armed state; away and vacation open
// it, night and home do not, a set-off alarm does.
func TestShutterFollowsAwayAlarmo(t *testing.T) {
	shutter := &fakeShutter{}
	follows := "away"
	runSteps(newTestShutterController(shutter, &follows), []step{
		{"disarmed", ""},                   // the start's
		{"arming", ""}, {"armed_away", ""}, // opens once armed
		{"disarmed", ""},
		{"arming", ""}, {"armed_night", ""}, {"disarmed", ""}, // never opens
		{"armed_vacation", ""}, {"disarmed", ""},
		{"armed_home", ""}, {"triggered", ""}, {"disarmed", ""},
	})
	want := []string{"open", "close", "open", "close", "open", "close"}
	if !slices.Equal(shutter.calls, want) {
		t.Errorf("shutter moves = %q, want %q", shutter.calls, want)
	}
}

// TestShutterStartMovesNothing: the state at start, restored or retained,
// leaves the shutter as saved, armed too.
func TestShutterStartMovesNothing(t *testing.T) {
	shutter := &fakeShutter{}
	follows := "all"
	c := newTestShutterController(shutter, &follows)

	runSteps(c, []step{{"armed_away", ""}, {"triggered", ""}})
	if len(shutter.calls) != 0 {
		t.Fatalf("shutter moved at start: %q", shutter.calls)
	}
	c.Handle("disarmed", "")
	if !slices.Equal(shutter.calls, []string{"close"}) {
		t.Errorf("shutter moves = %q, want close", shutter.calls)
	}
}

// TestShutterStartDuringAlarmoExitDelay: started during Alarmo's exit delay
// (where, away only, the shutter should be is not told), the armed state
// that follows moves the shutter.
func TestShutterStartDuringAlarmoExitDelay(t *testing.T) {
	shutter := &fakeShutter{}
	follows := "away"
	runSteps(newTestShutterController(shutter, &follows), []step{{"arming", ""}, {"armed_away", ""}})
	if !slices.Equal(shutter.calls, []string{"open"}) {
		t.Errorf("shutter moves = %q, want open", shutter.calls)
	}
}

// TestShutterOptionOff: off, the alarm moves nothing; turned on, the next
// change moves the shutter, without a restart, the same state again (Alarmo's
// at an MQTT reconnection) not being one.
func TestShutterOptionOff(t *testing.T) {
	shutter := &fakeShutter{}
	follows := "none"
	c := newTestShutterController(shutter, &follows)

	runSteps(c, []step{{"disarmed", ""}, {"arming", ""}, {"armed_away", ""}, {"disarmed", ""}, {"armed_night", ""}})
	if len(shutter.calls) != 0 {
		t.Fatalf("shutter moved with the option off: %q", shutter.calls)
	}
	follows = "all"
	runSteps(c, []step{{"armed_night", ""}, {"disarmed", ""}})
	if !slices.Equal(shutter.calls, []string{"close"}) {
		t.Errorf("shutter moves = %q, want close", shutter.calls)
	}
}

// TestShutterFailureNotRetried: a move that fails is not tried again
// until the alarm next changes.
func TestShutterFailureNotRetried(t *testing.T) {
	shutter := &fakeShutter{err: errors.New("radio down")}
	follows := "all"
	c := newTestShutterController(shutter, &follows)

	runSteps(c, []step{{"disarmed", ""}, {"arming", ""}, {"armed_away", ""}})
	shutter.err = nil
	c.Handle("disarmed", "")
	if want := []string{"open", "close"}; !slices.Equal(shutter.calls, want) {
		t.Errorf("shutter moves = %q, want %q", shutter.calls, want)
	}
}
