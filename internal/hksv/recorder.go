package hksv

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/caligone/openqiara/internal/aacenc"
	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/mediahub"
)

// liveBuffer is how far a recording may lag behind the camera, in
// samples (some 50 a second): past it, the controller can't keep up and
// the recording ends rather than skip frames inside a GOP.
const liveBuffer = 512

// errNotRecording: recording is off.
var errNotRecording = errors.New("hksv: recording is not active")

// Recorder keeps the prebuffer while recording is on and hands it, then
// the live stream, to a recording as fMP4.
type Recorder struct {
	log *slog.Logger

	mu     sync.Mutex
	hub    *mediahub.Hub
	active bool
	stop   context.CancelFunc // the hub subscription's; nil when not subscribed
	buf    prebuffer
	live   chan camera.Sample // the recording's samples; nil when none
}

// NewRecorder returns a recorder keeping prebufferMs of the camera.
func NewRecorder(prebufferMs int, logger *slog.Logger) *Recorder {
	return &Recorder{log: logger, buf: prebuffer{keep: int64(prebufferMs) * videoClock / 1000}}
}

// SetHub gives the recorder the camera; it comes after HomeKit starts.
func (r *Recorder) SetHub(h *mediahub.Hub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hub = h
	r.update()
}

// SetActive turns recording on or off: on, the recorder watches the
// camera to keep its prebuffer; off, it lets go of the camera.
func (r *Recorder) SetActive(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = on
	r.update()
}

// update subscribes to the hub or lets go of it, as active says.
func (r *Recorder) update() {
	switch want := r.active && r.hub != nil; {
	case want && r.stop == nil:
		ctx, cancel := context.WithCancel(context.Background())
		r.stop = cancel
		go r.watch(ctx, r.hub.Subscribe())
		r.log.Info("hksv: prebuffering")
	case !want && r.stop != nil:
		r.stop()
		r.stop = nil
		r.endLive()
		r.buf.reset()
		r.log.Info("hksv: prebuffer stopped")
	}
}

func (r *Recorder) watch(ctx context.Context, sub mediahub.Subscription) {
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-sub.Samples():
			if !ok {
				return
			}
			r.add(ctx, s)
		}
	}
}

func (r *Recorder) add(ctx context.Context, s camera.Sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return // a stale subscription's sample
	}
	r.buf.add(s)
	if r.live == nil {
		return
	}
	select {
	case r.live <- s:
	default:
		r.log.Warn("hksv: recording too slow, ending it")
		r.endLive()
	}
}

func (r *Recorder) endLive() {
	if r.live != nil {
		close(r.live)
		r.live = nil
	}
}

// Options are what iOS selected for a recording.
type Options struct {
	FragmentMs int
	Audio      bool // an AAC-LC 16 kHz track
	AudioKbps  int
}

// Record streams one recording: the prebuffer, then the camera live, as
// fMP4. emit gets the init segment, then each fragment; last marks the
// final one, when the camera's stream ended. Record returns when ctx ends
// (the controller closed the stream) or after the last fragment.
func (r *Recorder) Record(ctx context.Context, o Options, emit func(data []byte, last bool) error) error {
	r.mu.Lock()
	if r.stop == nil {
		r.mu.Unlock()
		return errNotRecording
	}
	if r.live != nil {
		r.mu.Unlock()
		return errors.New("hksv: a recording is already running")
	}
	past := r.buf.snapshot()
	live := make(chan camera.Sample, liveBuffer)
	r.live = live
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.live == live {
			r.endLive()
		}
		r.mu.Unlock()
	}()

	m := newMuxer(o.FragmentMs, r.audio(o))
	if m.aac != nil {
		defer m.aac.Close()
	}
	send := func(out [][]byte) error {
		for _, b := range out {
			if err := emit(b, false); err != nil {
				return err
			}
		}
		return nil
	}
	for _, s := range past {
		out, err := m.write(s)
		if err != nil {
			return err
		}
		if err := send(out); err != nil {
			return err
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s, ok := <-live:
			if !ok {
				return finish(m, send, emit)
			}
			out, err := m.write(s)
			if err != nil {
				return err
			}
			if err := send(out); err != nil {
				return err
			}
		}
	}
}

// finish sends what is left of a recording whose stream ended, its last
// fragment marked as such.
func finish(m *muxer, send func([][]byte) error, emit func([]byte, bool) error) error {
	out, err := m.flush()
	if err != nil {
		return err
	}
	if len(out) == 0 {
		return errors.New("hksv: the stream ended with nothing left to send")
	}
	if err := send(out[:len(out)-1]); err != nil {
		return err
	}
	return emit(out[len(out)-1], true)
}

// audio opens the recording's AAC encoder, nil when there is to be no
// audio track: off in Home, or no encoder on this build.
func (r *Recorder) audio(o Options) *aacenc.Stream {
	if !o.Audio {
		return nil
	}
	kbps := o.AudioKbps
	if kbps <= 0 {
		kbps = 24
	}
	aac, err := aacenc.NewStream(aacenc.LC, audioRate, kbps*1000)
	if err != nil {
		r.log.Warn("hksv: no AAC encoder, recording video only", "error", err)
		return nil
	}
	return aac
}
