package camera

import (
	"context"
	"os"
	"testing"
)

// TestParseRealChunk validates that the MPEG-TS parser can extract NAL
// units from a real chunk produced by hlcamd --use-h264. The chunk path
// is fixed to /tmp/sample.m4s — skip if not present.
//
// Run with: go test -v -run TestParseRealChunk ./internal/camera/
func TestParseRealChunk(t *testing.T) {
	data, err := os.ReadFile("/tmp/sample.m4s")
	if err != nil {
		t.Skip("no /tmp/sample.m4s on this host, skipping")
	}
	t.Logf("loaded sample: %d bytes", len(data))

	parser := NewMPEGTSParser(nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Drain samples in a goroutine, count NALs.
	var videoCount, audioCount int
	var videoSizes []int
	var nalTypes []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s := range parser.Samples() {
			if s.IsVideo {
				videoCount++
				videoSizes = append(videoSizes, len(s.Data))
				if len(s.Data) > 0 {
					nalTypes = append(nalTypes, s.Data[0]&0x1F)
				}
			} else {
				audioCount++
			}
			if videoCount > 100 {
				return // safety
			}
		}
	}()

	if _, err := parser.Feed(ctx, data); err != nil {
		t.Fatalf("Feed failed: %v", err)
	}
	if err := parser.Flush(ctx); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	parser.Close()
	<-done

	t.Logf("video samples: %d", videoCount)
	t.Logf("audio samples: %d", audioCount)
	t.Logf("first 10 NAL types: %v", nalTypes[:min(10, len(nalTypes))])
	t.Logf("first 10 NAL sizes: %v", videoSizes[:min(10, len(videoSizes))])

	if videoCount == 0 {
		t.Errorf("expected at least one video NAL, got 0")
	}
}

// TestParseRealChunkPTSDebug logs the full sequence of NAL types with
// their PTS values to detect whether hlcamd emits one PTS per frame
// (expected) or one PTS shared across all NAL of a chunk (broken).
func TestParseRealChunkPTSDebug(t *testing.T) {
	data, err := os.ReadFile("/tmp/sample.m4s")
	if err != nil {
		t.Skip("no /tmp/sample.m4s on this host, skipping")
	}

	parser := NewMPEGTSParser(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type entry struct {
		nalType byte
		pts     int64
		size    int
	}
	var entries []entry
	done := make(chan struct{})
	go func() {
		defer close(done)
		for s := range parser.Samples() {
			if !s.IsVideo {
				continue
			}
			nt := byte(0)
			if len(s.Data) > 0 {
				nt = s.Data[0] & 0x1F
			}
			entries = append(entries, entry{nalType: nt, pts: s.PTS, size: len(s.Data)})
			if len(entries) > 100 {
				return
			}
		}
	}()

	parser.Feed(ctx, data)
	parser.Flush(ctx)
	parser.Close()
	<-done

	if len(entries) == 0 {
		t.Fatal("no video NALs")
	}

	t.Logf("=== first 60 video NAL with PTS ===")
	limit := 60
	if len(entries) < limit {
		limit = len(entries)
	}
	prevPTS := int64(-2)
	transitions := 0
	for i, e := range entries[:limit] {
		marker := ""
		if e.pts != prevPTS {
			marker = " [PTS CHANGE]"
			if prevPTS >= 0 {
				transitions++
			}
			prevPTS = e.pts
		}
		t.Logf("  [%d] type=%d pts=%d size=%d%s", i, e.nalType, e.pts, e.size, marker)
	}
	t.Logf("=== %d unique PTS transitions in first %d NALs ===", transitions, limit)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
