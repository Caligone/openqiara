// Package hksv speaks HomeKit Secure Video: the recording configurations
// a camera offers and the one iOS selects (TLV8), and, later, the
// HomeKit Data Stream that carries the recordings. Reference: HAP-NodeJS,
// src/lib/camera/RecordingManagement.ts and src/lib/datastream/.
package hksv

import (
	"encoding/binary"
	"errors"
)

// item is one TLV8 entry.
type item struct {
	t byte
	v []byte
}

// encode writes items as TLV8: a value longer than 255 bytes goes in
// consecutive chunks of the same type.
func encode(items ...item) []byte {
	var b []byte
	for _, it := range items {
		v := it.v
		for {
			n := min(len(v), 255)
			b = append(b, it.t, byte(n))
			b = append(b, v[:n]...)
			v = v[n:]
			if len(v) == 0 {
				break
			}
		}
	}
	return b
}

// list repeats type t for each value, with the empty separator (type 0)
// HomeKit puts between entries of the same type.
func list(t byte, values ...[]byte) []item {
	var items []item
	for i, v := range values {
		if i > 0 {
			items = append(items, item{0, nil})
		}
		items = append(items, item{t, v})
	}
	return items
}

// decode reads TLV8, joining chunks of one value (a 255-byte chunk
// followed by one of the same type).
func decode(b []byte) ([]item, error) {
	var items []item
	for len(b) > 0 {
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			return nil, errors.New("hksv: tlv8 truncated")
		}
		t, v := b[0], b[2:2+int(b[1])]
		if n := len(items); n > 0 && items[n-1].t == t && t != 0 && len(items[n-1].v)%255 == 0 && len(items[n-1].v) > 0 {
			items[n-1].v = append(items[n-1].v, v...)
		} else {
			items = append(items, item{t, append([]byte(nil), v...)})
		}
		b = b[2+int(b[1]):]
	}
	return items, nil
}

// first returns the first value of type t.
func first(items []item, t byte) []byte {
	for _, it := range items {
		if it.t == t {
			return it.v
		}
	}
	return nil
}

func u8(v byte) []byte    { return []byte{v} }
func u16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }

// num reads a little-endian unsigned value of 1 to 8 bytes; 0 if absent.
func num(v []byte) uint64 {
	var n uint64
	for i := len(v) - 1; i >= 0; i-- {
		n = n<<8 | uint64(v[i])
	}
	return n
}
