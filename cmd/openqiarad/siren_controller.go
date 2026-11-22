package main

import (
	"context"
	"sync"
)

// alarmoSiren is what the mirror of Alarmo asks of the siren
// (alarm.SirenDriver).
type alarmoSiren interface {
	Arm(night, delayed bool)
	Disarm()
	EntryDelay()
	Alert()
}

// sirenController mirrors Alarmo's state on the siren, in alarmo mode:
// Alarmo decides and counts, the siren follows, armed natively so that it
// keeps the alarm by itself if the gateway goes. Its exit and entry delays
// are openqiara's (config): set them to Alarmo's, they only matter when
// the gateway is gone. The driver keeps the siren in the state asked for.
//
// Alarmo's states arrive on MQTT's goroutines: their commands go through
// one queue, in order. In tests, synchronous runs them inline.
type sirenController struct {
	commandQueue
	siren alarmoSiren

	mu   sync.Mutex
	last string // Alarmo's last state handled, "" before the first
}

func newSirenController(ctx context.Context, siren alarmoSiren) *sirenController {
	return &sirenController{commandQueue: newCommandQueue(ctx), siren: siren}
}

// nightStates are Alarmo's armed states where night-allowed sensors are
// left out; the others watch every sensor.
var nightStates = map[string]bool{"armed_night": true, "armed_home": true, "armed_custom_bypass": true}

func armedState(state string) bool {
	switch state {
	case "armed_away", "armed_vacation", "armed_night", "armed_home", "armed_custom_bypass":
		return true
	}
	return false
}

// Handle follows Alarmo's state. The first one is always taken, its
// retained state included: until then the siren driver wants nothing.
// Alarmo's exit delay does not say the mode to come: the siren is armed
// for away, and again for the mode once Alarmo is armed.
func (s *sirenController) Handle(newState string) {
	s.mu.Lock()
	same := newState == s.last
	s.last = newState
	s.mu.Unlock()
	if same {
		return
	}
	s.run(func() {
		switch {
		case newState == "disarmed":
			s.siren.Disarm()
		case newState == "arming":
			s.siren.Arm(false, true)
		case armedState(newState):
			s.siren.Arm(nightStates[newState], false)
		case newState == "pending":
			s.siren.EntryDelay()
		case newState == "triggered":
			s.siren.Alert()
		}
	})
}
