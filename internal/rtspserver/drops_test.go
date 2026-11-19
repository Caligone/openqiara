package rtspserver

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/bluenviron/gortsplib/v5"
)

// TestStreamWriteErrorSummary: dropped packets are counted, and the count is
// reset by the summary, logged at most every dropLogEvery.
func TestStreamWriteErrorSummary(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := &gortsplib.ServerHandlerOnStreamWriteErrorCtx{Error: errors.New("write queue is full")}
	s.OnStreamWriteError(ctx) // first one logs and resets
	s.OnStreamWriteError(ctx)
	s.OnStreamWriteError(ctx)
	if n := s.dropped.Load(); n != 2 {
		t.Fatalf("dropped = %d, want 2 (pending until the next summary)", n)
	}
}
