package rtspserver

import (
	"context"
	"os"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"

	"github.com/caligone/openqiara/internal/camera"
)

// TestPipelineEncodesRealSegment feeds a real MPEG-TS segment captured from
// the Qiara camera through the same NAL→AU→RTP path the RTSP server uses,
// and asserts we recover parameter sets, an IDR, and non-empty RTP packets.
// This exercises the whole chain offline (no camera, no network).
func TestPipelineEncodesRealSegment(t *testing.T) {
	data, err := os.ReadFile("testdata/sample_seg.ts")
	if err != nil {
		t.Skipf("no fixture: %v", err)
	}

	ctx := context.Background()
	parser := camera.NewMPEGTSParser(nil)

	// Drain samples in a goroutine, assembling access units by PTS exactly
	// like runPipeline does.
	type au struct {
		nals [][]byte
		pts  int64
	}
	aus := make(chan au, 64)
	go func() {
		defer close(aus)
		var cur [][]byte
		var curPTS int64
		have := false
		for s := range parser.Samples() {
			if !s.IsVideo {
				continue
			}
			if have && s.PTS != curPTS {
				aus <- au{nals: cur, pts: curPTS}
				cur = nil
			}
			curPTS = s.PTS
			have = true
			cur = append(cur, append([]byte(nil), s.Data...))
		}
		if have && len(cur) > 0 {
			aus <- au{nals: cur, pts: curPTS}
		}
	}()

	if _, err := parser.Feed(ctx, data); err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if err := parser.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	parser.Close()

	enc := &rtph264.Encoder{PayloadType: 96, PacketizationMode: 1}
	if err := enc.Init(); err != nil {
		t.Fatalf("encoder init: %v", err)
	}

	var sawSPS, sawPPS, sawIDR bool
	var totalPkts, totalAUs int
	for a := range aus {
		totalAUs++
		for _, nal := range a.nals {
			switch nalType(nal) {
			case nalTypeSPS:
				sawSPS = true
			case nalTypePPS:
				sawPPS = true
			case nalTypeIDR:
				sawIDR = true
			}
		}
		pkts, err := enc.Encode(a.nals)
		if err != nil {
			t.Fatalf("encode AU (pts=%d): %v", a.pts, err)
		}
		for _, p := range pkts {
			if len(p.Payload) == 0 {
				t.Errorf("empty RTP payload in AU pts=%d", a.pts)
			}
		}
		totalPkts += len(pkts)
	}

	if totalAUs == 0 {
		t.Fatal("no access units decoded from segment")
	}
	if !sawSPS || !sawPPS {
		t.Errorf("missing parameter sets: sps=%v pps=%v", sawSPS, sawPPS)
	}
	if !sawIDR {
		t.Error("no IDR frame in segment (expected at least one keyframe)")
	}
	if totalPkts == 0 {
		t.Error("no RTP packets produced")
	}
	t.Logf("decoded %d AUs → %d RTP packets (sps=%v pps=%v idr=%v)",
		totalAUs, totalPkts, sawSPS, sawPPS, sawIDR)
}
