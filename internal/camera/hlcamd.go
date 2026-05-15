package camera

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"time"
)

// HlcamdResumer wakes hlcamd up via fbxbus when its HLS pipeline stalls.
//
// hlcamd freeze silencieux après quelques heures : la playlist HLS n'est
// plus mise à jour mais le process reste up. `fbxbusctl call hlcamd
// resume_streams` débloque sans kill/restart. Plutôt qu'un watchdog
// polling, on déclenche le check de manière lazy à chaque requête video
// (handleHLSStream + HLSWatcher). En idle (personne ne regarde), aucun
// travail.
type HlcamdResumer struct {
	playlistPath string
	maxAge       time.Duration
	log          *slog.Logger

	// Anti-thundering-herd : si plusieurs requêtes parallèles détectent
	// le stale (cas burst HLS), on ne déclenche qu'un seul resume_streams.
	// Reset à 0 dès que le call est terminé — cooldown porte le minimum
	// d'intervalle entre 2 resume.
	inflight atomic.Bool
	lastCall atomic.Int64 // unix nanos
	cooldown time.Duration
}

// NewHlcamdResumer returns a resumer for the given HLS playlist path.
// maxAge is the staleness threshold (e.g. 10*time.Second). cooldown
// bounds the minimum interval between two resume calls (e.g. 5s) to
// avoid flooding fbxbusctl in case the pipeline is in a degraded state.
func NewHlcamdResumer(playlistPath string, maxAge, cooldown time.Duration, logger *slog.Logger) *HlcamdResumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &HlcamdResumer{
		playlistPath: playlistPath,
		maxAge:       maxAge,
		cooldown:     cooldown,
		log:          logger,
	}
}

// ResumeIfStale checks the playlist mtime and triggers resume_streams if
// it hasn't moved within maxAge. Returns true if a resume was issued.
//
// Best-effort : les erreurs (stat fail, fbxbusctl fail) sont loggées
// mais ne sont jamais remontées. Le caller continue à servir la requête
// HLS — si hlcamd ne revient pas, le client video verra un 404/timeout
// par le chemin normal.
func (r *HlcamdResumer) ResumeIfStale(ctx context.Context) bool {
	info, err := os.Stat(r.playlistPath)
	if err != nil {
		// Pas de playlist du tout = hlcamd n'a pas (encore) démarré
		// son pipeline. resume_streams peut aider.
		r.log.Debug("hls playlist missing — forcing resume", "path", r.playlistPath, "error", err)
		return r.callResume(ctx, "missing playlist")
	}
	age := time.Since(info.ModTime())
	if age < r.maxAge {
		return false
	}
	return r.callResume(ctx, fmt.Sprintf("playlist stale (%.1fs > %.1fs)", age.Seconds(), r.maxAge.Seconds()))
}

// ForceResume issues a resume regardless of staleness. Used by the
// explicit /api/stream/start path where the user signals video intent.
func (r *HlcamdResumer) ForceResume(ctx context.Context) error {
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
