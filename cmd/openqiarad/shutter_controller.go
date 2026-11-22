package main

import (
	"context"
	"log/slog"
	"sync"
)

// shutterController moves the privacy shutter with the alarm, as the config
// asks (alarm.shutter_follows, read at each change): open while the alarm
// watches the house, closed once disarmed, hlcamd's streams and vision
// analysis paused with it (#50). See shutterWant for where each state puts
// it.
//
// Only a change of where the shutter should be moves it: a shutter moved by
// hand stays so until then. The first state, the one restored or retained
// at start, moves nothing: the shutter's own state is saved
// (config.ShutterClosed).
//
// The states come from the standalone engine, under its lock, or from
// Alarmo on MQTT's goroutines; moving the shutter waits on the radio and
// on hlcamd, hence the queue.
type shutterController struct {
	commandQueue
	setShutter func(ctx context.Context, open bool) error
	follows    func() string // alarm.shutter_follows: "none", "away" or "all"
	log        *slog.Logger

	mu      sync.Mutex
	started bool   // a first state was seen
	last    string // the last state: the same again is not a change
	want    *bool  // where the shutter should be; nil: not known yet
}

func newShutterController(ctx context.Context, setShutter func(context.Context, bool) error, follows func() string, logger *slog.Logger) *shutterController {
	return &shutterController{commandQueue: newCommandQueue(ctx), setShutter: setShutter, follows: follows, log: logger}
}

// awayStates are the armed modes where nobody is home: the standalone
// engine's away, Alarmo's away and vacation.
var awayStates = map[string]bool{"armed_away": true, "armed_vacation": true}

// shutterWant tells where the shutter should be for an alarm state when it
// follows the alarm in some modes (follows): open, or closed; ok is false
// when the state does not tell.
//
//   - "all": open from the exit delay on, so that hlcamd has resumed by
//     the time the alarm is armed, in every armed mode;
//   - "away": open only armed away (awayStates); during the exit delay
//     when arming, the mode being armed, says so (the standalone engine
//     tells it, Alarmo does not: then it waits for the armed state);
//   - both: open once the alarm is set off (entry delay, alert), whatever
//     the mode, closed once disarmed;
//   - "none": never tells.
func shutterWant(follows, state, arming string) (open, ok bool) {
	if follows != "away" && follows != "all" {
		return false, false
	}
	switch {
	case state == "disarmed":
		return false, true
	case state == "pending", state == "triggered":
		return true, true
	case state == "arming":
		if follows == "all" {
			return true, true
		}
		if arming == "" {
			return false, false
		}
		return awayStates[arming], true
	case armedState(state):
		return follows == "all" || awayStates[state], true
	}
	return false, false
}

// Handle follows the alarm's state. arming is the mode being armed during
// the exit delay, "" if not known (Alarmo). The same state again (Alarmo's,
// retained, at each MQTT reconnection) changes nothing.
func (c *shutterController) Handle(state, arming string) {
	follows := c.follows()
	open, ok := shutterWant(follows, state, arming)
	c.mu.Lock()
	first, same := !c.started, c.started && state == c.last
	c.started, c.last = true, state
	prev := c.want
	switch {
	case same:
	case ok:
		c.want = &open
	case follows != "away" && follows != "all":
		// Not following: once it does, the next change moves the shutter.
		c.want = nil
	}
	c.mu.Unlock()
	if first || same || !ok || (prev != nil && *prev == open) {
		return
	}
	c.run(func() {
		if err := c.setShutter(c.ctx, open); err != nil {
			// An error, not a warning: open, the alarm is set without
			// image or detection; closed, the camera keeps filming while
			// disarmed.
			c.log.Error("shutter: following the alarm failed", "open", open, "state", state, "error", err)
			return
		}
		c.log.Info("shutter follows the alarm", "open", open, "state", state, "follows", follows)
	})
}
