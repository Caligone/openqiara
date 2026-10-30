package main

import (
	"context"
	"sync"
)

// commandQueue runs commands one at a time, in order, off the caller's
// goroutine: the alarm's states arrive under the engine's lock or on
// MQTT's goroutines, and a command may wait on the radio. In tests,
// synchronous runs them inline.
type commandQueue struct {
	ctx context.Context

	synchronous bool // tests only
	queue       chan func()
	startOnce   sync.Once
}

func newCommandQueue(ctx context.Context) commandQueue {
	return commandQueue{ctx: ctx, queue: make(chan func(), 16)}
}

// run runs fn in the queue, or inline in tests.
func (q *commandQueue) run(fn func()) {
	if q.synchronous {
		fn()
		return
	}
	q.startOnce.Do(func() {
		go func() {
			for {
				select {
				case fn := <-q.queue:
					fn()
				case <-q.ctx.Done():
					return
				}
			}
		}()
	})
	select {
	case q.queue <- fn:
	case <-q.ctx.Done():
	}
}
