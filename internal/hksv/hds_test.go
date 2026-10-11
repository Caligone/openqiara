package hksv

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// controller plays iOS's side of a data stream.
type controller struct {
	t           *testing.T
	nc          net.Conn
	read, write [32]byte // accessory-to-controller, controller-to-accessory
	rn, wn      uint64
}

func (c *controller) send(header, body map[string]any) {
	h, _ := encodeValue(nil, header)
	p, _ := encodeValue(append([]byte{byte(len(h))}, h...), body)
	fh := []byte{1, byte(len(p) >> 16), byte(len(p) >> 8), byte(len(p))}
	aead, _ := chacha20poly1305.New(c.write[:])
	if _, err := c.nc.Write(aead.Seal(append([]byte(nil), fh...), nonce(c.wn), p, fh)); err != nil {
		c.t.Fatal(err)
	}
	c.wn++
}

func (c *controller) recv() (map[string]any, map[string]any) {
	_ = c.nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	fh, body, err := readFrame(c.nc, maxFrame)
	if err != nil {
		c.t.Fatal(err)
	}
	aead, _ := chacha20poly1305.New(c.read[:])
	p, err := aead.Open(nil, nonce(c.rn), body, fh)
	if err != nil {
		c.t.Fatal(err)
	}
	c.rn++
	h, _, _ := decodeDict(p[1 : 1+int(p[0])])
	m, _, _ := decodeDict(p[1+int(p[0]):])
	return h, m
}

func TestDataStreamHelloThenRequest(t *testing.T) {
	got := make(chan map[string]any, 1)
	ds := NewDataStream(func(c *Conn) {
		h, m, err := c.Read()
		if err != nil {
			t.Error(err)
			return
		}
		got <- map[string]any{"header": h, "body": m}
		_ = c.Respond(h, 0, map[string]any{"status": int64(0)})
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer ds.Close()

	var shared [32]byte
	_, _ = rand.Read(shared[:])
	controllerSalt := make([]byte, 32)
	_, _ = rand.Read(controllerSalt)
	resp, err := ds.Setup(encode(item{0x01, u8(0)}, item{0x02, u8(0)}, item{0x03, controllerSalt}), shared)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := decode(resp)
	if s := first(items, 0x01); len(s) != 1 || s[0] != 0 {
		t.Fatalf("status % x", s)
	}
	params, _ := decode(first(items, 0x02))
	port := binary.LittleEndian.Uint16(first(params, 0x01))
	accessorySalt := first(items, 0x03)

	c := &controller{t: t}
	salt := append(append([]byte(nil), controllerSalt...), accessorySalt...)
	_ = derive(shared, salt, "HDS-Read-Encryption-Key", &c.read)
	_ = derive(shared, salt, "HDS-Write-Encryption-Key", &c.write)
	if c.nc, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(port))); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.nc.Close() }()

	c.send(map[string]any{"protocol": "control", "request": "hello", "id": int64(1)}, map[string]any{})
	if h, _ := c.recv(); h["response"] != "hello" || h["status"] != int64(0) || h["id"] != int64(1) {
		t.Fatalf("hello response %#v", h)
	}
	c.send(map[string]any{"protocol": "dataSend", "request": "open", "id": int64(2)},
		map[string]any{"target": "controller", "type": "ipcamera.recording", "streamId": int64(7)})
	select {
	case m := <-got:
		if m["body"].(map[string]any)["streamId"] != int64(7) {
			t.Fatalf("open %#v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request not handed over")
	}
	if h, b := c.recv(); h["response"] != "open" || b["status"] != int64(0) {
		t.Fatalf("open response %#v %#v", h, b)
	}

	// A stranger's connection, keyed by no prepared session, is dropped.
	nc, _ := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(port)))
	stranger := &controller{t: t, nc: nc}
	stranger.send(map[string]any{"protocol": "control", "request": "hello", "id": int64(1)}, map[string]any{})
	_ = nc.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Fatal("stranger got an answer")
	}
}

// Before authentication, a frame larger than a hello is refused, not
// buffered.
func TestOversizedFirstFrame(t *testing.T) {
	ds := NewDataStream(func(*Conn) {}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer ds.Close()
	salt := make([]byte, 32)
	resp, err := ds.Setup(encode(item{0x01, u8(0)}, item{0x02, u8(0)}, item{0x03, salt}), [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := decode(resp)
	params, _ := decode(first(items, 0x02))
	nc, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(binary.LittleEndian.Uint16(first(params, 0x01)))))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := nc.Write([]byte{1, 0x0f, 0xff, 0xff}); err != nil {
		t.Fatal(err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := nc.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection kept open: %v", err)
	}
}
