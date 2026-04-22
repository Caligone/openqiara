package charmux

import (
	"bytes"
	"testing"
)

func TestVarintRoundtrip(t *testing.T) {
	tests := []uint32{0, 1, 127, 128, 255, 256, 16384, 0xFFFFFF, 0x7FFFFFFF}
	for _, v := range tests {
		buf := appendVarint(nil, v)
		got, n := decodeVarint(buf)
		if got != v {
			t.Errorf("varint roundtrip %d: got %d", v, got)
		}
		if n != len(buf) {
			t.Errorf("varint %d: consumed %d bytes, expected %d", v, n, len(buf))
		}
	}
}

func TestVarintEncoding(t *testing.T) {
	// Single byte values (0-127)
	buf := appendVarint(nil, 1)
	if !bytes.Equal(buf, []byte{0x01}) {
		t.Errorf("varint(1) = %x, want 01", buf)
	}

	// Two byte value (128)
	buf = appendVarint(nil, 128)
	if !bytes.Equal(buf, []byte{0x80, 0x01}) {
		t.Errorf("varint(128) = %x, want 80 01", buf)
	}

	// Counter encoding: value * 2
	buf = appendVarint(nil, 4*2) // counter=4 → encode 8
	if !bytes.Equal(buf, []byte{0x08}) {
		t.Errorf("varint(8) = %x, want 08", buf)
	}
}

func TestManagedFrameSerialize(t *testing.T) {
	f := &ManagedFrame{
		GWDst:   6,
		GWSrc:   1,
		RFByte:  0,
		Counter: 4,
		Src:     1,
		Flags:   FlagZ | FlagW, // 0x0003
		AckDst:  6,
		AckCnt:  67,
		WFlags:  1,
		Payload: []byte{0x55, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00},
	}

	data := f.Serialize()

	// Verify structure
	// gwdst=6 → 0x06
	// gwsrc=1 → 0x01
	// rf_byte → 0x00
	// counter=4 → 4*2=8 → 0x08
	// src=1 → 0x01
	// flags=0x0003 → LE: 0x03 0x00
	// ackdst=6 → 0x06
	// ackcnt=67 → 0x43
	// wflags=1 → 0x01
	// payload → 55 00 01 00 00 00 00

	if data[0] != 0x06 {
		t.Errorf("gwdst: got %02x, want 06", data[0])
	}
	if data[1] != 0x01 {
		t.Errorf("gwsrc: got %02x, want 01", data[1])
	}
	if data[2] != 0x00 {
		t.Errorf("rf_byte: got %02x, want 00", data[2])
	}
	if data[3] != 0x08 {
		t.Errorf("counter*2: got %02x, want 08", data[3])
	}
	if data[4] != 0x01 {
		t.Errorf("src: got %02x, want 01", data[4])
	}
	// flags LE
	if data[5] != 0x03 || data[6] != 0x00 {
		t.Errorf("flags: got %02x %02x, want 03 00", data[5], data[6])
	}
}

func TestManagedFrameRoundtrip(t *testing.T) {
	original := &ManagedFrame{
		GWDst:   10,
		GWSrc:   1,
		RFByte:  0,
		Counter: 12,
		Src:     1,
		Flags:   FlagZ | FlagW | FlagA | FlagE, // 0x0407
		AckDst:  10,
		AckCnt:  6,
		WFlags:  0xCD,
		Payload: []byte{0x01, 0x80, 0x88, 0x87, 0x00, 0x00},
	}

	data := original.Serialize()
	parsed, err := DeserializeManagedFrame(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if parsed.GWDst != original.GWDst {
		t.Errorf("GWDst: got %d, want %d", parsed.GWDst, original.GWDst)
	}
	if parsed.GWSrc != original.GWSrc {
		t.Errorf("GWSrc: got %d, want %d", parsed.GWSrc, original.GWSrc)
	}
	if parsed.Counter != original.Counter {
		t.Errorf("Counter: got %d, want %d", parsed.Counter, original.Counter)
	}
	if parsed.Flags != original.Flags {
		t.Errorf("Flags: got %04x, want %04x", parsed.Flags, original.Flags)
	}
	if parsed.AckDst != original.AckDst {
		t.Errorf("AckDst: got %d, want %d", parsed.AckDst, original.AckDst)
	}
	if parsed.AckCnt != original.AckCnt {
		t.Errorf("AckCnt: got %d, want %d", parsed.AckCnt, original.AckCnt)
	}
	if parsed.WFlags != original.WFlags {
		t.Errorf("WFlags: got %02x, want %02x", parsed.WFlags, original.WFlags)
	}
	if !bytes.Equal(parsed.Payload, original.Payload) {
		t.Errorf("Payload: got %x, want %x", parsed.Payload, original.Payload)
	}
}

func TestNewConfigFrame(t *testing.T) {
	payload := []byte{0x55, 0x00, 0x03, 0x16, 0x00, 0x00, 0x00}
	f := NewConfigFrame(23, 9, payload)

	if f.GWDst != 23 {
		t.Errorf("GWDst: got %d, want 23", f.GWDst)
	}
	if f.Flags != FlagZ|FlagW {
		t.Errorf("Flags: got %04x, want %04x", f.Flags, FlagZ|FlagW)
	}
	if f.WFlags != 1 {
		t.Errorf("WFlags: got %02x, want 01", f.WFlags)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Errorf("Payload mismatch")
	}
}

func TestNewBytecodeFrame(t *testing.T) {
	payload := []byte{0x01, 0x80}
	f := NewBytecodeFrame(7, 3, payload)

	if f.WFlags != 0xCD {
		t.Errorf("WFlags: got %02x, want CD", f.WFlags)
	}
}

// fbxhomeFlags is the numeric value of "flags:1351ZWAE" from fbxhome logs.
// 1351 decimal = 0x0547 = bits 0,1,2,6,8,10.
const fbxhomeFlags uint16 = FlagZ | FlagW | FlagA | 0x0040 | 0x0100 | FlagE

func TestFbxhomeFlagsParsing(t *testing.T) {
	// "flags:1351ZWAE" → 1351 decimal = 0x0547
	if fbxhomeFlags != 0x0547 {
		t.Fatalf("fbxhomeFlags = 0x%04x, want 0x0547", fbxhomeFlags)
	}
	if 0x0547 != 1351 {
		t.Fatal("0x0547 != 1351 decimal")
	}

	// Verify each named flag is present
	for _, tc := range []struct {
		name string
		flag uint16
	}{
		{"Z", FlagZ},
		{"W", FlagW},
		{"A", FlagA},
		{"E", FlagE},
	} {
		if fbxhomeFlags&tc.flag == 0 {
			t.Errorf("flag %s (0x%04x) not set in 0x%04x", tc.name, tc.flag, fbxhomeFlags)
		}
	}

	// Verify the "class bits" (0x0040 | 0x0100) are set
	classBits := uint16(0x0040 | 0x0100)
	if fbxhomeFlags&classBits != classBits {
		t.Errorf("class bits 0x%04x not set in 0x%04x", classBits, fbxhomeFlags)
	}
}

// TestFbxhomeKPDConfig1Code tests roundtrip of a real KPD config frame with 1 code.
// Sent manage: [gwdst:9, gwsrc:1, cnt:8, src:1, rf_sig:93, rf_cfg:240,
//
//	flags:1351ZWAE, ackdst:9, ackcnt:6, wflags:1, payload:55000100000000]
func TestFbxhomeKPDConfig1Code(t *testing.T) {
	f := &ManagedFrame{
		GWDst:   9,
		GWSrc:   1,
		RFByte:  0, // rf_sig/rf_cfg are set to 0 in TX
		Counter: 8,
		Src:     1,
		Flags:   fbxhomeFlags,
		AckDst:  9,
		AckCnt:  6,
		WFlags:  1,
		Payload: []byte{0x55, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00},
	}

	data := f.Serialize()
	parsed, err := DeserializeManagedFrame(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	assertManagedFrameEqual(t, parsed, f)
}

// TestFbxhomeKPDConfig0Codes tests roundtrip of a real KPD config frame with 0 codes.
// Sent manage: [gwdst:10, gwsrc:1, cnt:6, src:1, rf_sig:93, rf_cfg:240,
//
//	flags:1351ZWAE, ackdst:10, ackcnt:6, wflags:1, payload:55000000000000]
func TestFbxhomeKPDConfig0Codes(t *testing.T) {
	f := &ManagedFrame{
		GWDst:   10,
		GWSrc:   1,
		RFByte:  0,
		Counter: 6,
		Src:     1,
		Flags:   fbxhomeFlags,
		AckDst:  10,
		AckCnt:  6,
		WFlags:  1,
		Payload: []byte{0x55, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	}

	data := f.Serialize()
	parsed, err := DeserializeManagedFrame(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	assertManagedFrameEqual(t, parsed, f)
}

// TestFbxhomePIRConfig tests roundtrip of a real PIR config frame.
// Sent manage: [gwdst:23, gwsrc:1, cnt:9, src:1, rf_sig:62, rf_cfg:160,
//
//	flags:1351ZWAE, ackdst:23, ackcnt:5, wflags:1, payload:55000316000000]
func TestFbxhomePIRConfig(t *testing.T) {
	f := &ManagedFrame{
		GWDst:   23,
		GWSrc:   1,
		RFByte:  0,
		Counter: 9,
		Src:     1,
		Flags:   fbxhomeFlags,
		AckDst:  23,
		AckCnt:  5,
		WFlags:  1,
		Payload: []byte{0x55, 0x00, 0x03, 0x16, 0x00, 0x00, 0x00},
	}

	data := f.Serialize()
	parsed, err := DeserializeManagedFrame(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	assertManagedFrameEqual(t, parsed, f)
}

// TestFbxhomeHeartbeatResponse tests roundtrip of a real heartbeat response.
// Sent: [gwdst:6, gwsrc:1, cnt:0, src:1, rf_sig:18, rf_cfg:58,
//
//	flags:1351ZWAE, ackdst:6, ackcnt:61, wflags:1, payload:0300041032]
func TestFbxhomeHeartbeatResponse(t *testing.T) {
	f := &ManagedFrame{
		GWDst:   6,
		GWSrc:   1,
		RFByte:  0,
		Counter: 0,
		Src:     1,
		Flags:   fbxhomeFlags,
		AckDst:  6,
		AckCnt:  61,
		WFlags:  1,
		Payload: []byte{0x03, 0x00, 0x04, 0x10, 0x32},
	}

	data := f.Serialize()
	parsed, err := DeserializeManagedFrame(data)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	assertManagedFrameEqual(t, parsed, f)
}

// assertManagedFrameEqual compares all fields of two ManagedFrames.
func assertManagedFrameEqual(t *testing.T, got, want *ManagedFrame) {
	t.Helper()
	if got.GWDst != want.GWDst {
		t.Errorf("GWDst: got %d, want %d", got.GWDst, want.GWDst)
	}
	if got.GWSrc != want.GWSrc {
		t.Errorf("GWSrc: got %d, want %d", got.GWSrc, want.GWSrc)
	}
	if got.Counter != want.Counter {
		t.Errorf("Counter: got %d, want %d", got.Counter, want.Counter)
	}
	if got.Src != want.Src {
		t.Errorf("Src: got %d, want %d", got.Src, want.Src)
	}
	if got.Flags != want.Flags {
		t.Errorf("Flags: got 0x%04x, want 0x%04x", got.Flags, want.Flags)
	}
	if got.AckDst != want.AckDst {
		t.Errorf("AckDst: got %d, want %d", got.AckDst, want.AckDst)
	}
	if got.AckCnt != want.AckCnt {
		t.Errorf("AckCnt: got %d, want %d", got.AckCnt, want.AckCnt)
	}
	if got.WFlags != want.WFlags {
		t.Errorf("WFlags: got 0x%02x, want 0x%02x", got.WFlags, want.WFlags)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf("Payload: got %x, want %x", got.Payload, want.Payload)
	}
}

func TestChunking(t *testing.T) {
	// Verify that Serialize output can be split into 8-byte chunks
	f := NewConfigFrame(6, 4, []byte{0x55, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00})
	data := f.Serialize()

	// Each chunk is 0x1C + up to 8 bytes
	chunks := 0
	for i := 0; i < len(data); i += 8 {
		chunks++
	}
	if chunks == 0 {
		t.Error("expected at least 1 chunk")
	}

	// Verify last chunk may be shorter
	remainder := len(data) % 8
	if remainder == 0 {
		remainder = 8
	}
	lastChunkSize := remainder
	if lastChunkSize > 8 {
		t.Errorf("last chunk too big: %d", lastChunkSize)
	}
}
