package camera

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HLSWatcher follows an HLS playlist on disk (the one produced by hlcamd
// in /tmp/out_stream/stream/720p/HLS_TEST.m3u8) and emits the bytes of
// each new segment file as soon as it's complete.
//
// Designed for the HomeKit camera streaming pipeline: a single watcher
// per active streaming session, started when iOS hits the Start command,
// stopped on End.
//
// HLSWatcher is a one-shot: call Run with a context and consume Chunks
// until ctx is cancelled. Don't reuse a HLSWatcher across sessions.
type HLSWatcher struct {
	playlistPath string
	logger       *slog.Logger

	// pollInterval controls how often we re-scan the playlist for new
	// segments. The Qiara camera produces 1-second segments, so 200 ms
	// gives us snappy detection without thrashing the disk.
	pollInterval time.Duration

	// stableDelay is how long we wait after a segment file is first
	// observed before we consider it complete. The Qiara camera writes
	// each segment in roughly 100 ms; 250 ms is a safe margin.
	stableDelay time.Duration

	chunks chan []byte

	// Track segments we've already emitted so a m3u8 reload doesn't
	// re-emit old ones.
	mu      sync.Mutex
	emitted map[string]bool
}

// NewHLSWatcher returns a watcher for the given playlist file.
// The playlist path is the full path to a .m3u8 file (typically
// /tmp/out_stream/stream/720p/HLS_TEST.m3u8 on the Qiara camera).
func NewHLSWatcher(playlistPath string, logger *slog.Logger) *HLSWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	return &HLSWatcher{
		playlistPath: playlistPath,
		logger:       logger,
		pollInterval: 200 * time.Millisecond,
		stableDelay:  250 * time.Millisecond,
		chunks:       make(chan []byte, 4),
		emitted:      make(map[string]bool),
	}
}

// Chunks returns a receive-only channel of complete MPEG-TS chunks. The
// channel is closed when Run returns. Each chunk is the raw content of a
// .m4s file (despite the extension, hlcamd writes raw MPEG-TS in there,
// not fragmented MP4).
func (w *HLSWatcher) Chunks() <-chan []byte {
	return w.chunks
}

// Run blocks until ctx is cancelled, polling the playlist and emitting
// new chunks on the Chunks() channel. Always close(chunks) before
// returning so consumers know to stop.
func (w *HLSWatcher) Run(ctx context.Context) error {
	defer close(w.chunks)

	w.logger.Info("hls watcher: starting", "playlist", w.playlistPath, "poll", w.pollInterval)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("hls watcher: context cancelled, stopping")
			return nil
		case <-ticker.C:
			if err := w.pollOnce(ctx); err != nil {
				// Don't bail on transient errors — hlcamd might be
				// briefly down between segment writes. Just log and
				// keep polling.
				w.logger.Warn("hls watcher: poll failed", "error", err)
			}
		}
	}
}

// pollOnce reads the playlist, checks for new segments, and emits them.
func (w *HLSWatcher) pollOnce(ctx context.Context) error {
	segments, err := w.readPlaylist()
	if err != nil {
		return fmt.Errorf("read playlist: %w", err)
	}

	dir := filepath.Dir(w.playlistPath)
	for _, seg := range segments {
		w.mu.Lock()
		seen := w.emitted[seg]
		w.mu.Unlock()
		if seen {
			continue
		}

		segPath := filepath.Join(dir, seg)
		data, err := w.readStableFile(ctx, segPath)
		if err != nil {
			w.logger.Debug("hls watcher: segment not yet stable",
				"segment", seg, "error", err)
			continue
		}
		if len(data) == 0 {
			// Empty segment — hlcamd creates the file before writing
			// to it. Skip and let next poll catch it.
			continue
		}

		w.mu.Lock()
		w.emitted[seg] = true
		w.mu.Unlock()

		// Garbage-collect the emitted set so it doesn't grow forever.
		// Keep only segments still referenced in the playlist.
		w.gcEmitted(segments)

		select {
		case w.chunks <- data:
			w.logger.Debug("hls watcher: chunk emitted", "segment", seg, "size", len(data))
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// readPlaylist parses the m3u8 file and returns the list of segment
// filenames in order. Comments and tags are ignored.
func (w *HLSWatcher) readPlaylist() ([]string, error) {
	f, err := os.Open(w.playlistPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var segments []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		segments = append(segments, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return segments, nil
}

// readStableFile reads a file but only returns its content if the file
// has not changed size for at least w.stableDelay. This avoids reading
// a half-written segment.
func (w *HLSWatcher) readStableFile(ctx context.Context, path string) ([]byte, error) {
	info1, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info1.Size() == 0 {
		return nil, nil
	}

	select {
	case <-time.After(w.stableDelay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	info2, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info2.Size() != info1.Size() {
		return nil, fmt.Errorf("file still growing (%d → %d)", info1.Size(), info2.Size())
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// gcEmitted removes from the emitted set any segment that is no longer
// referenced in the current playlist (i.e. it has rotated out).
func (w *HLSWatcher) gcEmitted(current []string) {
	currentSet := make(map[string]bool, len(current))
	for _, s := range current {
		currentSet[s] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.emitted {
		if !currentSet[k] {
			delete(w.emitted, k)
		}
	}
}
