package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// shutterSetTimeout bounds one shutter move.
const shutterSetTimeout = 10 * time.Second

// shutterFollower keeps the privacy shutter in step with the alarm state,
// off the caller's goroutine: the latest wanted position wins.
type shutterFollower struct {
	set     func(ctx context.Context, open bool) error
	enabled func() bool
	logger  *slog.Logger

	mu      sync.Mutex
	last    *bool // last position requested; nil when unknown
	pending chan bool
}

func newShutterFollower(ctx context.Context, set func(ctx context.Context, open bool) error, enabled func() bool, logger *slog.Logger) *shutterFollower {
	f := &shutterFollower{
		set:     set,
		enabled: enabled,
		logger:  logger,
		pending: make(chan bool, 1),
	}
	go f.run(ctx)
	return f
}

// AlarmState never blocks: "disarmed" wants the shutter closed, any other
// state (arming, armed_away, armed_night, pending, triggered) wants it open.
// It does nothing while enabled() is false, and nothing when the wanted
// position equals the last one requested.
func (f *shutterFollower) AlarmState(state string) {
	if !f.enabled() {
		return
	}
	open := state != "disarmed"

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.last != nil && *f.last == open {
		return
	}
	f.last = &open
	// Replace any waiting request: the latest wins. Every channel
	// operation is non-blocking, and the mutex serializes the producers.
	select {
	case <-f.pending:
	default:
	}
	select {
	case f.pending <- open:
	default:
	}
}

func (f *shutterFollower) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case open := <-f.pending:
			f.apply(ctx, open)
		}
	}
}

func (f *shutterFollower) apply(ctx context.Context, open bool) {
	cctx, cancel := context.WithTimeout(ctx, shutterSetTimeout)
	defer cancel()
	err := f.set(cctx, open)
	if err == nil {
		return
	}
	f.logger.Warn("shutter follow failed", "open", open, "error", err)
	// Unknown position: let the next identical request retry.
	f.mu.Lock()
	if f.last != nil && *f.last == open {
		f.last = nil
	}
	f.mu.Unlock()
}
