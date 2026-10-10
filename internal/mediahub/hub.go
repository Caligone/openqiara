// Package mediahub fans out the camera's H.264 sample stream to multiple
// consumers (HomeKit SRTP, RTSP, HLS) from a single source: what hlcamd
// multicasts on the loopback, its 1080p stream and its microphone.
//
// The Hub reads the source once, on demand: it starts when the first
// subscriber attaches and stops when the last one leaves.
//
// Fan-out is lossy per subscriber: each subscription has a bounded buffer
// and drops its oldest samples if that consumer can't keep up, so one slow
// reader (e.g. RTSP over a congested link) never stalls the others or the
// parser.
package mediahub

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caligone/openqiara/internal/camera"
)

// subBuffer bounds each subscriber's queue. At ~30 fps a full buffer is
// ~1s of video; past that the consumer is hopelessly behind and dropping
// the oldest sample is the right call.
const subBuffer = 32

// Resumer wakes hlcamd if its stream has gone stale. HomeKit's
// camera path already relies on this before consuming chunks; the Hub does
// the same so the first subscriber gets frames promptly. nil is fine.
type Resumer interface {
	// ResumeIfStale wakes the pipeline if stale. The bool return (did it
	// resume) is unused here; we match camera.HlcamdResumer's signature.
	ResumeIfStale(ctx context.Context) bool
}

// Source starts the camera's sample stream; the channel closes when ctx
// ends or the source fails.
type Source func(ctx context.Context) (<-chan camera.Sample, error)

// Multicast reads what hlcamd multicasts on the loopback: the H.264 stream
// on videoPort (1080p on camera.MulticastVideoMain) and the microphone's
// PCM on audioPort.
func Multicast(videoPort, audioPort int, logger *slog.Logger) Source {
	return func(ctx context.Context) (<-chan camera.Sample, error) {
		video, err := camera.MulticastVideo(ctx, videoPort, logger)
		if err != nil {
			return nil, err
		}
		audio, err := camera.MulticastPCM(ctx, audioPort, logger)
		if err != nil {
			logger.Warn("mediahub: no audio", "error", err)
			return video, nil
		}
		return merge(ctx, video, audio), nil
	}
}

// merge forwards both channels' samples until both close.
func merge(ctx context.Context, a, b <-chan camera.Sample) <-chan camera.Sample {
	out := make(chan camera.Sample)
	var wg sync.WaitGroup
	for _, in := range []<-chan camera.Sample{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range in {
				select {
				case out <- s:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

// Hub multiplexes one camera pipeline to many subscribers.
type Hub struct {
	name   string // for logs
	source Source
	// staleCheck is how often a running pipeline asks the resumer whether
	// the stream stalled.
	staleCheck time.Duration
	log        *slog.Logger
	resumer    Resumer

	last atomic.Int64 // unix nanos of the last sample

	mu     sync.Mutex
	subs   map[*subscription]struct{}
	cancel context.CancelFunc // stops the running pipeline; nil when idle
}

type subscription struct {
	ch  chan camera.Sample
	hub *Hub
}

// New returns an idle Hub. The pipeline starts on the first Subscribe;
// name tells the source in logs.
func New(name string, source Source, resumer Resumer, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		name:       name,
		source:     source,
		staleCheck: 5 * time.Second,
		log:        logger,
		resumer:    resumer,
		subs:       make(map[*subscription]struct{}),
	}
}

// Subscription is the consumer-facing handle. Read Samples until it closes
// (Hub shutdown) and call Close when done to release the pipeline.
type Subscription struct{ s *subscription }

// Samples returns the receive-only sample channel for this subscription.
func (s Subscription) Samples() <-chan camera.Sample { return s.s.ch }

// Close detaches the subscription. Stopping the last subscriber stops the
// shared pipeline. Safe to call more than once.
func (s Subscription) Close() { s.s.hub.unsubscribe(s.s) }

// Subscribe attaches a consumer, starting the shared pipeline if it was
// idle. The returned Subscription's Samples() channel receives every
// video/audio sample the parser emits (subject to lossy backpressure).
func (h *Hub) Subscribe() Subscription {
	sub := &subscription{ch: make(chan camera.Sample, subBuffer), hub: h}

	h.mu.Lock()
	h.subs[sub] = struct{}{}
	first := len(h.subs) == 1
	if first {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go h.runPipeline(ctx)
	}
	h.mu.Unlock()

	if first {
		h.log.Info("mediahub: pipeline started", "source", h.name)
	}
	return Subscription{s: sub}
}

func (h *Hub) unsubscribe(sub *subscription) {
	h.mu.Lock()
	if _, ok := h.subs[sub]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, sub)
	close(sub.ch)
	var cancel context.CancelFunc
	if len(h.subs) == 0 {
		cancel = h.cancel
		h.cancel = nil
	}
	h.mu.Unlock()

	if cancel != nil {
		cancel()
		h.log.Info("mediahub: pipeline stopped (no subscribers)")
	}
}

// LastSample tells when the source last sent a sample; zero: never.
func (h *Hub) LastSample() time.Time {
	if n := h.last.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// broadcast delivers a sample to every subscriber, dropping it for any
// subscriber whose buffer is full rather than blocking the parser.
func (h *Hub) broadcast(sample camera.Sample) {
	h.last.Store(time.Now().UnixNano())
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs {
		select {
		case sub.ch <- sample:
		default:
			// Consumer is behind; drop this sample for it. Video-only
			// consumers recover at the next IDR (which re-carries SPS/PPS).
		}
	}
}

// restartDelay spaces the source's restarts after it failed.
const restartDelay = time.Second

// runPipeline runs the source and broadcasts each sample. A source that
// fails is started again while someone listens: a subscriber that stays
// (HomeKit Secure Video's prebuffer) would otherwise keep a dead pipeline
// for everyone.
func (h *Hub) runPipeline(ctx context.Context) {
	for {
		h.runSource(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(restartDelay):
			h.log.Info("mediahub: restarting the source", "source", h.name)
		}
	}
}

// runSource runs the source once.
func (h *Hub) runSource(ctx context.Context) {
	if h.resumer != nil {
		h.resumer.ResumeIfStale(ctx)
	}
	samples, err := h.source(ctx)
	if err != nil {
		h.log.Warn("mediahub: source failed", "source", h.name, "error", err)
		return
	}
	check := time.NewTicker(h.staleCheck)
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-check.C:
			if h.resumer != nil {
				h.resumer.ResumeIfStale(ctx)
			}
		case sample, ok := <-samples:
			if !ok {
				return
			}
			h.broadcast(sample)
		}
	}
}
