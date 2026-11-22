package camera

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// HlcamdResumer wakes hlcamd up via fbxbus when its stream stalls.
//
// hlcamd freeze silencieux après quelques heures : plus d'image mais le
// process reste up. `fbxbusctl call hlcamd resume_streams` débloque sans
// kill/restart. Plutôt qu'un watchdog polling, on déclenche le check de
// manière lazy quand quelqu'un regarde (mediahub). En idle, aucun travail.
type HlcamdResumer struct {
	// lastFrame tells when the stream last moved: the media hub's last
	// sample. Zero: never.
	lastFrame func() time.Time
	maxAge    time.Duration
	log       *slog.Logger

	// Anti-thundering-herd : si plusieurs requêtes parallèles détectent
	// le stale (cas burst HLS), on ne déclenche qu'un seul resume_streams.
	// Reset à 0 dès que le call est terminé — cooldown porte le minimum
	// d'intervalle entre 2 resume.
	inflight atomic.Bool
	lastCall atomic.Int64 // unix nanos
	cooldown time.Duration

	// closed is the privacy shutter: closed, the streams stay paused.
	closed atomic.Bool
}

// NewHlcamdResumer returns a resumer judging the stream by lastFrame.
// maxAge is the staleness threshold (e.g. 10*time.Second). cooldown
// bounds the minimum interval between two resume calls (e.g. 5s) to
// avoid flooding fbxbusctl in case the pipeline is in a degraded state.
func NewHlcamdResumer(lastFrame func() time.Time, maxAge, cooldown time.Duration, logger *slog.Logger) *HlcamdResumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &HlcamdResumer{
		lastFrame: lastFrame,
		maxAge:    maxAge,
		cooldown:  cooldown,
		log:       logger,
	}
}

// ResumeIfStale triggers resume_streams if the stream hasn't moved within
// maxAge. Returns true if a resume was issued.
//
// Best-effort : les erreurs fbxbusctl sont loggées mais ne sont jamais
// remontées. Si hlcamd ne revient pas, le client vidéo verra un timeout
// par le chemin normal.
func (r *HlcamdResumer) ResumeIfStale(ctx context.Context) bool {
	if r.closed.Load() {
		return false // paused on purpose: shutter closed
	}
	last := r.lastFrame()
	if last.IsZero() {
		return r.callResume(ctx, "no frame yet")
	}
	age := time.Since(last)
	if age < r.maxAge {
		return false
	}
	return r.callResume(ctx, fmt.Sprintf("stream stale (%.1fs > %.1fs)", age.Seconds(), r.maxAge.Seconds()))
}

// ForceResume issues a resume regardless of staleness. Used by the
// explicit /api/stream/start path where the user signals video intent.
func (r *HlcamdResumer) ForceResume(ctx context.Context) error {
	if r.closed.Load() {
		return fmt.Errorf("resume_streams skipped: shutter closed")
	}
	if !r.callResume(ctx, "forced") {
		return fmt.Errorf("resume_streams skipped (cooldown or inflight)")
	}
	return nil
}

// callResume runs the fbxbusctl call. Returns false if skipped (cooldown
// or another goroutine in flight), true if executed (success or failure
// — both are logged).
func (r *HlcamdResumer) callResume(ctx context.Context, reason string) bool {
	now := time.Now().UnixNano()
	last := r.lastCall.Load()
	if last > 0 && time.Duration(now-last) < r.cooldown {
		return false
	}
	if !r.inflight.CompareAndSwap(false, true) {
		return false
	}
	defer r.inflight.Store(false)
	r.lastCall.Store(now)

	cmd := exec.CommandContext(ctx, "fbxbusctl", "call", "hlcamd", "resume_streams")
	if err := cmd.Run(); err != nil {
		r.log.Warn("hlcamd resume_streams failed", "reason", reason, "error", err)
		return true
	}
	r.log.Info("hlcamd resume_streams issued", "reason", reason)
	return true
}

// Shutter follows the privacy shutter: closed, hlcamd's streams are paused,
// its vision analysis with them (about 68 % of a core down to 5 %, measured
// 2026-11-19 on rc.25, #50), and nothing resumes them until it opens. hlcamd
// stays alive for watchdog_mcu. Also called at start: hlcamd starts paused,
// and may not be on fbxbus yet, hence the retries.
func (r *HlcamdResumer) Shutter(ctx context.Context, open bool) {
	r.closed.Store(!open)
	method := "resume_streams"
	if !open {
		method = "pause_streams"
	}
	for try := 1; ; try++ {
		out, err := exec.CommandContext(ctx, "fbxbusctl", "call", "hlcamd", method).CombinedOutput()
		if err == nil {
			r.log.Info("hlcamd "+method+" issued", "reason", "shutter")
			return
		}
		if try == 6 || ctx.Err() != nil {
			r.log.Warn("hlcamd "+method+" failed", "error", err, "output", strings.TrimSpace(string(out)))
			return
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}
