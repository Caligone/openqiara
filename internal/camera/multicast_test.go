package camera

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

// fragments cuts a frame the way hlcamd does (24-byte header, then data).
func fragments(ts uint64, seq uint32, frame []byte, size int) [][]byte {
	var out [][]byte
	for off := 0; off < len(frame); off += size {
		end := min(off+size, len(frame))
		p := make([]byte, multicastHeader+end-off)
		binary.LittleEndian.PutUint64(p[0:], ts)
		binary.LittleEndian.PutUint32(p[10:], seq)
		binary.LittleEndian.PutUint32(p[14:], uint32(off))
		binary.LittleEndian.PutUint32(p[18:], uint32(len(frame)))
		binary.LittleEndian.PutUint16(p[22:], uint16(end-off))
		copy(p[multicastHeader:], frame[off:end])
		out = append(out, p)
	}
	return out
}

func TestFrameAssembler(t *testing.T) {
	frame := bytes.Repeat([]byte{0, 0, 0, 1, 0x65, 1, 2, 3}, 100)
	var a frameAssembler
	frags := fragments(1_000_000, 7, frame, 300)
	for i, p := range frags {
		f, err := parseMulticastFragment(p)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := a.add(f)
		if ok != (i == len(frags)-1) {
			t.Fatalf("fragment %d: complete=%v", i, ok)
		}
		if ok && !bytes.Equal(got, frame) {
			t.Fatal("frame differs")
		}
	}
}

func TestFrameAssemblerDropsIncomplete(t *testing.T) {
	var a frameAssembler
	first := fragments(1, 1, make([]byte, 600), 300)
	second := fragments(2, 2, []byte{9, 9, 9}, 300)
	f, _ := parseMulticastFragment(first[0]) // the rest of frame 1 is lost
	if _, ok := a.add(f); ok {
		t.Fatal("incomplete frame returned")
	}
	f, _ = parseMulticastFragment(second[0])
	got, ok := a.add(f)
	if !ok || !bytes.Equal(got, []byte{9, 9, 9}) {
		t.Fatalf("next frame = %v %v", got, ok)
	}
}

func TestParseMulticastFragmentBounds(t *testing.T) {
	p := fragments(1, 1, []byte{1, 2, 3}, 300)[0]
	binary.LittleEndian.PutUint16(p[22:], 10)
	if _, err := parseMulticastFragment(p); err == nil {
		t.Fatal("data_size past the packet accepted")
	}
	if _, err := parseMulticastFragment(p[:10]); err == nil {
		t.Fatal("short packet accepted")
	}
	q := fragments(1, 1, []byte{1, 2, 3}, 300)[0]
	binary.LittleEndian.PutUint32(q[18:], 1<<30)
	if _, err := parseMulticastFragment(q); err == nil {
		t.Fatal("oversized frame accepted")
	}
	binary.LittleEndian.PutUint32(q[18:], maxFrameSize)
	if _, err := parseMulticastFragment(q); err != nil {
		t.Fatalf("frame at the limit refused: %v", err)
	}
}

func TestReadMulticastVideo(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	samples := readMulticast(ctx, conn, "video", slog.New(slog.NewTextHandler(io.Discard, nil)), emitNALs)

	tx, err := net.Dial("udp4", conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0, 0, 0, 1, 0x67, 0x42, 0x1f, 0, 0, 0, 1, 0x65, 0xaa}
	for _, p := range fragments(2_000_000, 3, frame, 5) {
		if _, err := tx.Write(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range [][]byte{{0x67, 0x42, 0x1f}, {0x65, 0xaa}} {
		select {
		case s := <-samples:
			if !s.IsVideo || s.PTS != 180_000 || !bytes.Equal(s.Data, want) {
				t.Fatalf("sample = %+v, want NAL % x", s, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no sample")
		}
	}
	cancel()
	for range samples { // closes once ctx ends
	}
}
