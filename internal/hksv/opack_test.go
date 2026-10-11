package hksv

import (
	"bytes"
	"reflect"
	"testing"
)

// Bytes worked out from HAP-NodeJS's DataStreamParser tags.
func TestEncodeHelloHeader(t *testing.T) {
	b, err := encodeValue(nil, map[string]any{"protocol": "control", "response": "hello", "id": int64(1), "status": int64(0)})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xe4,
		0x42, 'i', 'd', 0x09, // id: 1
		0x48, 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l', 0x47, 'c', 'o', 'n', 't', 'r', 'o', 'l',
		0x48, 'r', 'e', 's', 'p', 'o', 'n', 's', 'e', 0x45, 'h', 'e', 'l', 'l', 'o',
		0x46, 's', 't', 'a', 't', 'u', 's', 0x08, // status: 0
	}
	if !bytes.Equal(b, want) {
		t.Fatalf("\n got % x\nwant % x", b, want)
	}
}

func TestRoundTrip(t *testing.T) {
	long := bytes.Repeat([]byte{1}, 300)
	in := map[string]any{
		"streamId":    int64(1234),
		"endOfStream": true,
		"neg":         int64(-1),
		"big":         int64(1 << 40),
		"f":           1.5,
		"packets": []any{
			map[string]any{"data": long, "metadata": map[string]any{"dataType": "mediaFragment", "dataSequenceNumber": int64(1)}},
		},
		"null": nil,
	}
	b, err := encodeValue(nil, in)
	if err != nil {
		t.Fatal(err)
	}
	out, rest, err := decodeDict(append(b, 0xaa))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) || !bytes.Equal(rest, []byte{0xaa}) {
		t.Fatalf("round trip\n got %#v\nwant %#v", out, in)
	}
}

// iOS may refer back to a string it already sent.
func TestBackReference(t *testing.T) {
	// {"a": "xy", "b": <ref 1>}: tracked are "a" (0), "xy" (1), "b" (2).
	b := []byte{0xe2, 0x41, 'a', 0x42, 'x', 'y', 0x41, 'b', 0xa1}
	m, _, err := decodeDict(b)
	if err != nil {
		t.Fatal(err)
	}
	if m["b"] != "xy" {
		t.Fatalf("back-reference gave %#v", m["b"])
	}
	if _, _, err := decodeDict([]byte{0xe1, 0x41, 'a', 0xa9}); err == nil {
		t.Fatal("dangling back-reference accepted")
	}
}
