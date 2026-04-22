package charmux

import (
	"context"
	"encoding/binary"
	"fmt"
)

// ManagedFrame represents a DomusRF managed frame sent to sensors via PKT.
type ManagedFrame struct {
	GWDst   uint32 // Destination address (sensor)
	GWSrc   uint32 // Source address (1 = gateway)
	RFByte  byte   // Radio config, always 0 for TX
	Counter uint32 // Frame counter
	Src     uint32 // Source ID
	Flags   uint16 // Frame flags (see FlagXxx constants)
	AckDst  uint32 // ACK destination / wait_src
	AckCnt  uint32 // ACK counter / wait_cnt
	WFlags  byte   // Wake flags (only if FlagW set)
	Payload []byte // Payload data (only if FlagW set)
}

// Flag bits for ManagedFrame.Flags.
const (
	FlagZ uint16 = 1 << 0  // Type bit 0
	FlagW uint16 = 1 << 1  // WAKE / has payload
	FlagA uint16 = 1 << 2  // ACK
	FlagU uint16 = 1 << 3  //
	FlagP uint16 = 1 << 4  //
	FlagT uint16 = 1 << 5  //
	FlagE uint16 = 1 << 10 //
)

// Serialize encodes the managed frame into its binary wire format.
func (f *ManagedFrame) Serialize() []byte {
	return f.serialize()
}

func (f *ManagedFrame) serialize() []byte {
	var buf []byte

	buf = appendVarint(buf, f.GWDst)
	buf = appendVarint(buf, f.GWSrc)
	buf = append(buf, f.RFByte)
	buf = appendVarint(buf, f.Counter*2) // counter is LSL 1
	buf = appendVarint(buf, f.Src)

	// flags: uint16 little-endian (NOT varint)
	var flagBytes [2]byte
	binary.LittleEndian.PutUint16(flagBytes[:], f.Flags)
	buf = append(buf, flagBytes[:]...)

	if f.Flags&FlagA != 0 {
		buf = appendVarint(buf, f.AckDst)
		buf = appendVarint(buf, f.AckCnt)
	}

	if f.Flags&FlagW != 0 {
		buf = append(buf, f.WFlags)
		buf = append(buf, f.Payload...)
	}

	return buf
}

// Deserialize parses a managed frame from its binary wire format.
func DeserializeManagedFrame(data []byte) (*ManagedFrame, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("managed frame too short: %d bytes", len(data))
	}

	f := &ManagedFrame{}
	pos := 0

	var n int
	f.GWDst, n = decodeVarint(data[pos:])
	pos += n

	f.GWSrc, n = decodeVarint(data[pos:])
	pos += n

	if pos >= len(data) {
		return nil, fmt.Errorf("managed frame truncated at rf_byte")
	}
	f.RFByte = data[pos]
	pos++

	cnt, n := decodeVarint(data[pos:])
	f.Counter = cnt / 2 // counter is LSR 1
	pos += n

	f.Src, n = decodeVarint(data[pos:])
	pos += n

	if pos+2 > len(data) {
		return nil, fmt.Errorf("managed frame truncated at flags")
	}
	f.Flags = binary.LittleEndian.Uint16(data[pos : pos+2])
	pos += 2

	// AckDst/AckCnt are only present when the A flag is set.
	if f.Flags&FlagA != 0 {
		f.AckDst, n = decodeVarint(data[pos:])
		pos += n

		f.AckCnt, n = decodeVarint(data[pos:])
		pos += n
	}

	if f.Flags&FlagW != 0 && pos < len(data) {
		f.WFlags = data[pos]
		pos++
		if pos < len(data) {
			f.Payload = make([]byte, len(data)-pos)
			copy(f.Payload, data[pos:])
		}
	}

	return f, nil
}

// SendManaged serializes a managed frame and sends it on the PKT channel
// using the 0x1C + 8-byte chunk protocol.
func (c *Client) SendManaged(ctx context.Context, frame *ManagedFrame) error {
	data := frame.Serialize()

	// Send in 8-byte chunks, each prefixed with 0x1C
	for i := 0; i < len(data); i += 8 {
		end := i + 8
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, 1+end-i)
		chunk[0] = 0x1C
		copy(chunk[1:], data[i:end])

		if _, err := c.pktConn.Write(chunk); err != nil {
			return fmt.Errorf("send managed chunk: %w", err)
		}
	}

	// Send end marker
	if _, err := c.pktConn.Write([]byte{0x1D}); err != nil {
		return fmt.Errorf("send managed end: %w", err)
	}

	return nil
}

// NewConfigFrame creates a managed frame for sending config to a sensor.
// flags = FlagZ | FlagW (0x03), wflags = 1.
func NewConfigFrame(dst uint32, counter uint32, payload []byte) *ManagedFrame {
	return &ManagedFrame{
		GWDst:   dst,
		GWSrc:   1,
		RFByte:  0,
		Counter: counter,
		Src:     1,
		Flags:   FlagZ | FlagW, // 0x03
		AckDst:  dst,
		AckCnt:  0,
		WFlags:  1,
		Payload: payload,
	}
}

// NewBytecodeFrame creates a managed frame for uploading bytecode to a sensor.
// flags = FlagZ | FlagW (0x03), wflags = 0xCD.
func NewBytecodeFrame(dst uint32, counter uint32, payload []byte) *ManagedFrame {
	return &ManagedFrame{
		GWDst:   dst,
		GWSrc:   1,
		RFByte:  0,
		Counter: counter,
		Src:     1,
		Flags:   FlagZ | FlagW, // 0x03
		AckDst:  dst,
		AckCnt:  0,
		WFlags:  0xCD,
		Payload: payload,
	}
}

// --- varint encoding (protobuf-style) ---

func appendVarint(buf []byte, v uint32) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	buf = append(buf, byte(v))
	return buf
}

// DecodeVarint decodes a protobuf-style varint from data.
// Returns the value and number of bytes consumed.
func DecodeVarint(data []byte) (uint32, int) {
	return decodeVarint(data)
}

func decodeVarint(data []byte) (uint32, int) {
	var v uint32
	var shift uint
	for i, b := range data {
		v |= uint32(b&0x7F) << shift
		if b&0x80 == 0 {
			return v, i + 1
		}
		shift += 7
		if shift >= 32 {
			return v, i + 1
		}
	}
	return v, len(data)
}
