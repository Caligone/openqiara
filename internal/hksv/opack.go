package hksv

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// The HomeKit Data Stream's messages are dictionaries in Apple's compact
// binary encoding (HAP-NodeJS DataStreamParser.ts). Go values:
// map[string]any, []any, string, []byte, int64, float64, bool, nil.
// The encoder never writes back-references; the decoder follows them.

// encodeValue appends v.
func encodeValue(b []byte, v any) ([]byte, error) {
	switch v := v.(type) {
	case nil:
		return append(b, 0x04), nil
	case bool:
		if v {
			return append(b, 0x01), nil
		}
		return append(b, 0x02), nil
	case int:
		return encodeInt(b, int64(v)), nil
	case int64:
		return encodeInt(b, v), nil
	case float64:
		b = append(b, 0x36)
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(v)), nil
	case string:
		return encodeBytes(b, []byte(v), 0x40, 0x61), nil
	case []byte:
		return encodeBytes(b, v, 0x70, 0x91), nil
	case []any:
		if len(v) <= 12 {
			b = append(b, 0xd0+byte(len(v)))
		} else {
			b = append(b, 0xdf)
		}
		for _, e := range v {
			var err error
			if b, err = encodeValue(b, e); err != nil {
				return nil, err
			}
		}
		if len(v) > 12 {
			b = append(b, 0x03)
		}
		return b, nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic bytes, for tests
		if len(keys) <= 14 {
			b = append(b, 0xe0+byte(len(keys)))
		} else {
			b = append(b, 0xef)
		}
		for _, k := range keys {
			b = encodeBytes(b, []byte(k), 0x40, 0x61)
			var err error
			if b, err = encodeValue(b, v[k]); err != nil {
				return nil, err
			}
		}
		if len(keys) > 14 {
			b = append(b, 0x03)
		}
		return b, nil
	}
	return nil, fmt.Errorf("hds: cannot encode %T", v)
}

func encodeInt(b []byte, n int64) []byte {
	switch {
	case n == -1:
		return append(b, 0x07)
	case n >= 0 && n <= 39:
		return append(b, 0x08+byte(n))
	case n >= math.MinInt8 && n <= math.MaxInt8:
		return append(b, 0x30, byte(int8(n)))
	case n >= math.MinInt16 && n <= math.MaxInt16:
		return binary.LittleEndian.AppendUint16(append(b, 0x31), uint16(int16(n)))
	case n >= math.MinInt32 && n <= math.MaxInt32:
		return binary.LittleEndian.AppendUint32(append(b, 0x32), uint32(int32(n)))
	}
	return binary.LittleEndian.AppendUint64(append(b, 0x33), uint64(n))
}

// encodeBytes writes a string or data: inline length up to 32 (tag
// inline+len), else a 1, 2 or 4-byte length after tags long..long+2.
func encodeBytes(b, v []byte, inline, long byte) []byte {
	switch n := len(v); {
	case n <= 32:
		b = append(b, inline+byte(n))
	case n <= math.MaxUint8:
		b = append(b, long, byte(n))
	case n <= math.MaxUint16:
		b = binary.LittleEndian.AppendUint16(append(b, long+1), uint16(n))
	default:
		b = binary.LittleEndian.AppendUint32(append(b, long+2), uint32(n))
	}
	return append(b, v...)
}

// decoder reads values, keeping the primitives it saw: tags 0xa0..0xcf
// refer back to them by position (HAP-NodeJS tracks the same kinds).
type decoder struct {
	b       []byte
	tracked []any
}

var errShort = errors.New("hds: message truncated")

func (d *decoder) take(n int) ([]byte, error) {
	if n < 0 || len(d.b) < n {
		return nil, errShort
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v, nil
}

func (d *decoder) track(v any) any {
	d.tracked = append(d.tracked, v)
	return v
}

func (d *decoder) value() (any, error) {
	t, err := d.take(1)
	if err != nil {
		return nil, err
	}
	tag := t[0]
	switch {
	case tag == 0x01:
		return d.track(true), nil
	case tag == 0x02:
		return d.track(false), nil
	case tag == 0x04:
		return nil, nil
	case tag == 0x05:
		u, err := d.take(16)
		if err != nil {
			return nil, err
		}
		return d.track(append([]byte(nil), u...)), nil
	case tag == 0x06, tag == 0x36:
		f, err := d.take(8)
		if err != nil {
			return nil, err
		}
		return d.track(math.Float64frombits(binary.LittleEndian.Uint64(f))), nil
	case tag == 0x35:
		f, err := d.take(4)
		if err != nil {
			return nil, err
		}
		return d.track(float64(math.Float32frombits(binary.LittleEndian.Uint32(f)))), nil
	case tag == 0x07:
		return d.track(int64(-1)), nil
	case tag >= 0x08 && tag <= 0x2f:
		return d.track(int64(tag - 0x08)), nil
	case tag >= 0x30 && tag <= 0x33:
		n := 1 << (tag - 0x30)
		v, err := d.take(n)
		if err != nil {
			return nil, err
		}
		var x int64
		switch n {
		case 1:
			x = int64(int8(v[0]))
		case 2:
			x = int64(int16(binary.LittleEndian.Uint16(v)))
		case 4:
			x = int64(int32(binary.LittleEndian.Uint32(v)))
		case 8:
			x = int64(binary.LittleEndian.Uint64(v))
		}
		return d.track(x), nil
	case tag >= 0x40 && tag <= 0x60:
		s, err := d.take(int(tag - 0x40))
		if err != nil {
			return nil, err
		}
		return d.track(string(s)), nil
	case tag >= 0x61 && tag <= 0x64:
		s, err := d.lengthPrefixed(tag - 0x61)
		if err != nil {
			return nil, err
		}
		return d.track(string(s)), nil
	case tag >= 0x70 && tag <= 0x90:
		v, err := d.take(int(tag - 0x70))
		if err != nil {
			return nil, err
		}
		return d.track(append([]byte(nil), v...)), nil
	case tag >= 0x91 && tag <= 0x94:
		v, err := d.lengthPrefixed(tag - 0x91)
		if err != nil {
			return nil, err
		}
		return d.track(append([]byte(nil), v...)), nil
	case tag >= 0xa0 && tag <= 0xcf:
		i := int(tag - 0xa0)
		if i >= len(d.tracked) {
			return nil, fmt.Errorf("hds: back-reference %d of %d", i, len(d.tracked))
		}
		return d.tracked[i], nil
	case tag >= 0xd0 && tag <= 0xdf:
		var arr []any
		for i := 0; tag == 0xdf || i < int(tag-0xd0); i++ {
			if tag == 0xdf && len(d.b) > 0 && d.b[0] == 0x03 {
				d.b = d.b[1:]
				break
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		return arr, nil
	case tag >= 0xe0 && tag <= 0xef:
		m := map[string]any{}
		for i := 0; tag == 0xef || i < int(tag-0xe0); i++ {
			if tag == 0xef && len(d.b) > 0 && d.b[0] == 0x03 {
				d.b = d.b[1:]
				break
			}
			k, err := d.value()
			if err != nil {
				return nil, err
			}
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("hds: dictionary key %T", k)
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			m[ks] = v
		}
		return m, nil
	}
	return nil, fmt.Errorf("hds: unknown tag %#x", tag)
}

// lengthPrefixed reads a 1, 2, 4 or 8-byte little-endian length (size 0..3)
// and that many bytes.
func (d *decoder) lengthPrefixed(size byte) ([]byte, error) {
	l, err := d.take(1 << size)
	if err != nil {
		return nil, err
	}
	var n uint64
	for i := len(l) - 1; i >= 0; i-- {
		n = n<<8 | uint64(l[i])
	}
	if n > uint64(len(d.b)) {
		return nil, errShort
	}
	return d.take(int(n))
}

// decodeDict reads one dictionary from the start of b and returns the rest.
func decodeDict(b []byte) (map[string]any, []byte, error) {
	d := &decoder{b: b}
	v, err := d.value()
	if err != nil {
		return nil, nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("hds: %T where a dictionary was expected", v)
	}
	return m, d.b, nil
}
