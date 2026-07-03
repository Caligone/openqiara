// Package mediahub fans out the camera's H.264 sample stream to multiple
// consumers (HomeKit SRTP, RTSP, …) from a single HLS→MPEG-TS pipeline.
//
// Without it, each output built its own HLSWatcher + MPEGTSParser, so N
// active outputs meant N disk readers and N MPEG-TS decodes of the very
// same segments — wasteful on the camera's already-loaded SoC. The Hub
// runs that pipeline exactly once, on demand: it starts when the first
// subscriber attaches and stops when the last one leaves, mirroring the
// per-output on-demand behaviour that used to live in each consumer.
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

	"github.com/caligone/openqiara/internal/camera"
)

// subBuffer bounds each subscriber's queue. At ~30 fps a full buffer is
// ~1s of video; past that the consumer is hopelessly behind and dropping
// the oldest sample is the right call.
const subBuffer = 32

// Resumer wakes the HLS pipeline (hlcamd) if it has gone stale. HomeKit's
// camera path already relies on this before consuming chunks; the Hub does
// the same so the first subscriber gets frames promptly. nil is fine.
type Resumer interface {
	// ResumeIfStale wakes the pipeline if stale. The bool return (did it
	// resume) is unused here; we match camera.HlcamdResumer's signature.
	ResumeIfStale(ctx context.Context) bool
}

// Hub multiplexes one camera pipeline to many subscribers.
type Hub struct {
	hlsPath string
	log     *slog.Logger
	resumer Resumer

	mu     sync.Mutex
	subs   map[*subscription]struct{}
	cancel context.CancelFunc // stops the running pipeline; nil when idle
}

type subscription struct {
	ch  chan camera.Sample
	hub *Hub
}

// New returns an idle Hub. The pipeline starts on the first Subscribe.
func New(hlsPath string, resumer Resumer, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		hlsPath: hlsPath,
		log:     logger,
		resumer: resumer,
		subs:    make(map[*subscription]struct{}),
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
		h.log.Info("mediahub: pipeline started", "hls", h.hlsPath)
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

// broadcast delivers a sample to every subscriber, dropping it for any
// subscriber whose buffer is full rather than blocking the parser.
func (h *Hub) broadcast(sample camera.Sample) {
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

// runPipeline drives HLSWatcher → MPEGTSParser once and broadcasts each
// emitted sample. It mirrors the wiring that previously lived in every
// consumer, including the flush-after-chunk that trims ~1s of latency.
func (h *Hub) runPipeline(ctx context.Context) {
	if h.resumer != nil {
		h.resumer.ResumeIfStale(ctx)
	}

	watcher := camera.NewHLSWatcher(h.hlsPath, h.log)
	parser := camera.NewMPEGTSParser(h.log)

	go func() {
		if err := watcher.Run(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("mediahub: hls watcher exited", "error", err)
		}
	}()

	go func() {
		defer parser.Close()
		for {
			select {
			case <-ctx.Done():
				_ = parser.Flush(context.Background())
				return
			case chunk, ok := <-watcher.Chunks():
				if !ok {
					_ = parser.Flush(context.Background())
					return
				}
				if _, err := parser.Feed(ctx, chunk); err != nil && ctx.Err() == nil {
					h.log.Warn("mediahub: mpegts feed error", "error", err)
				}
				// Flush per chunk so the last PES is emitted promptly
				// instead of waiting for the next chunk (~1s saved).
				if err := parser.Flush(ctx); err != nil && ctx.Err() == nil {
					h.log.Warn("mediahub: mpegts flush error", "error", err)
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case sample, ok := <-parser.Samples():
			if !ok {
				return
			}
			h.broadcast(sample)
		}
	}
}
