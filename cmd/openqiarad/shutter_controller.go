package main

import (
	"context"
	"log/slog"
	"sync"
)

// shutterController moves the privacy shutter with the alarm, when the
// config asks it to (alarm.shutter_follows_alarm): open from the exit
// delay on, so that hlcamd has resumed by the time the alarm is armed,
// closed once disarmed, hlcamd's streams and vision analysis paused with
// it (#50). Every armed mode opens it, night included.
//
// Only a change between disarmed and set moves it: a shutter moved by hand
// stays so until the alarm next arms or disarms. The first state, the one
// restored or retained at start, moves nothing: the shutter's own state is
// saved (config.ShutterClosed).
//
// The states come from the standalone engine, under its lock, or from
// Alarmo on MQTT's goroutines; moving the shutter waits on the radio and
// on hlcamd, hence the queue.
type shutterController struct {
	commandQueue
	setShutter func(ctx context.Context, open bool) error
	enabled    func() bool // the option, read at each change
	log        *slog.Logger

	mu  sync.Mutex
	set *bool // whether the alarm was last set; nil before the first state
}

func newShutterController(ctx context.Context, setShutter func(context.Context, bool) error, enabled func() bool, logger *slog.Logger) *shutterController {
	return &shutterController{commandQueue: newCommandQueue(ctx), setShutter: setShutter, enabled: enabled, log: logger}
}

// alarmSet tells whether a state has the alarm set: its exit delay, an
// armed mode (the standalone engine's or Alarmo's), its entry delay or set
// off. ok is false for a state that says neither.
func alarmSet(state string) (set, ok bool) {
	switch {
	case state == "disarmed":
		return false, true
	case state == "arming", state == "pending", state == "triggered", armedState(state):
		return true, true
	}
	return false, false
}

// Handle follows the alarm's state: the shutter opens when it is set,
// closes when it is disarmed.
func (c *shutterController) Handle(state string) {
	set, ok := alarmSet(state)
	if !ok {
		return
	}
	c.mu.Lock()
	prev := c.set
	c.set = &set
	c.mu.Unlock()
	if prev == nil || *prev == set || !c.enabled() {
		return
	}
	c.run(func() {
		if err := c.setShutter(c.ctx, set); err != nil {
			// An error, not a warning: open, the alarm is set without
			// image or detection; closed, the camera keeps filming while
			// disarmed.
			c.log.Error("shutter: following the alarm failed", "open", set, "state", state, "error", err)
			return
		}
		c.log.Info("shutter follows the alarm", "open", set, "state", state)
	})
}
