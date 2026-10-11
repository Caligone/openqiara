package hksv

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// The HomeKit Data Stream (HAP-NodeJS DataStreamServer.ts): when iOS
// wants to talk to the camera (recordings), it writes
// SetupDataStreamTransport with its salt; the camera answers with a TCP
// port and its own salt. iOS connects within 10 s. Both directions are
// ChaCha20-Poly1305 frames, keyed by HKDF-SHA512 over the HAP session's
// Pair-Verify secret, salted with both salts.

const (
	prepareTimeout = 10 * time.Second
	writeTimeout   = 5 * time.Second
	maxFrame       = 1<<20 - 1
	maxHello       = 4 << 10 // a hello is ~50 bytes: refuse more before authentication
	maxUnmatched   = 4       // connections not yet matched to a session
)

// DataStream accepts iOS's data stream connections.
type DataStream struct {
	log    *slog.Logger
	handle func(*Conn) // a connection that said hello

	mu        sync.Mutex
	ln        net.Listener
	pending   []*prepared
	unmatched int
}

type prepared struct {
	read, write [32]byte // controller-to-accessory, accessory-to-controller
	deadline    time.Time
}

// NewDataStream returns a data stream server; handle gets each connection
// once its hello is answered, and owns it.
func NewDataStream(handle func(*Conn), logger *slog.Logger) *DataStream {
	return &DataStream{handle: handle, log: logger}
}

// Setup answers a SetupDataStreamTransport write: req is iOS's TLV, shared
// its HAP session's Pair-Verify secret. The answer carries the listening
// port and the accessory's salt.
func (s *DataStream) Setup(req []byte, shared [32]byte) ([]byte, error) {
	items, err := decode(req)
	if err != nil {
		return nil, err
	}
	if c := first(items, 0x01); len(c) != 1 || c[0] != 0 {
		return nil, fmt.Errorf("hds: setup command % x, only start (0) known", c)
	}
	controllerSalt := first(items, 0x03)
	if len(controllerSalt) != 32 {
		return nil, fmt.Errorf("hds: controller salt of %d bytes", len(controllerSalt))
	}
	accessorySalt := make([]byte, 32)
	if _, err := rand.Read(accessorySalt); err != nil {
		return nil, err
	}
	salt := append(append([]byte(nil), controllerSalt...), accessorySalt...)
	p := &prepared{deadline: time.Now().Add(prepareTimeout)}
	if err := derive(shared, salt, "HDS-Write-Encryption-Key", &p.read); err != nil {
		return nil, err
	}
	if err := derive(shared, salt, "HDS-Read-Encryption-Key", &p.write); err != nil {
		return nil, err
	}
	port, err := s.listen()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.pending = append(s.pending, p)
	s.mu.Unlock()
	return encode(
		item{0x01, u8(0)}, // status: success
		item{0x02, encode(item{0x01, u16(uint16(port))})},
		item{0x03, accessorySalt},
	), nil
}

func derive(secret [32]byte, salt []byte, info string, key *[32]byte) error {
	_, err := io.ReadFull(hkdf.New(sha512.New, secret[:], salt, []byte(info)), key[:])
	return err
}

// listen starts the TCP listener once and returns its port.
func (s *DataStream) listen() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			return 0, err
		}
		s.ln = ln
		go s.accept(ln)
	}
	return s.ln.Addr().(*net.TCPAddr).Port, nil
}

// Close stops the listener.
func (s *DataStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
		s.ln = nil
	}
}

func (s *DataStream) accept(ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		go s.serve(nc)
	}
}

// serve matches a connection to a prepared session by its first frame,
// which must be the hello, then hands it over.
func (s *DataStream) serve(nc net.Conn) {
	s.mu.Lock()
	full := s.unmatched >= maxUnmatched
	if !full {
		s.unmatched++
	}
	s.mu.Unlock()
	if full {
		_ = nc.Close()
		return
	}
	_ = nc.SetReadDeadline(time.Now().Add(prepareTimeout))
	header, body, err := readFrame(nc, maxHello)
	s.mu.Lock()
	s.unmatched--
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("hds: no first frame", "error", err)
		_ = nc.Close()
		return
	}
	c := s.match(nc, header, body)
	if c == nil {
		s.log.Warn("hds: first frame from no prepared session", "remote", nc.RemoteAddr())
		_ = nc.Close()
		return
	}
	_ = nc.SetReadDeadline(time.Time{})
	h, _, err := c.decodePayload(c.firstPlain)
	if err != nil || h["protocol"] != "control" || h["request"] != "hello" {
		s.log.Warn("hds: first message is not a hello", "header", h, "error", err)
		_ = nc.Close()
		return
	}
	if err := c.Respond(h, 0, map[string]any{}); err != nil {
		_ = nc.Close()
		return
	}
	s.log.Info("hds: connected", "remote", nc.RemoteAddr())
	s.handle(c)
}

func (s *DataStream) match(nc net.Conn, header, body []byte) *Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for i := 0; i < len(s.pending); i++ {
		p := s.pending[i]
		if now.After(p.deadline) {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			i--
			continue
		}
		c := &Conn{nc: nc, read: p.read, write: p.write}
		if plain, err := c.open(header, body); err == nil {
			c.readN++
			c.firstPlain = plain
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return c
		}
	}
	return nil
}

// Conn is one data stream connection.
type Conn struct {
	nc          net.Conn
	read, write [32]byte
	readN       uint64
	firstPlain  []byte

	wmu    sync.Mutex
	writeN uint64
}

// readFrame reads one frame of at most limit bytes: a 4-byte header (type 1,
// 24-bit big-endian plaintext length), the ciphertext and its 16-byte tag.
func readFrame(r io.Reader, limit int) (header, body []byte, err error) {
	header = make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, nil, err
	}
	if header[0] != 1 {
		return nil, nil, fmt.Errorf("hds: frame type %d", header[0])
	}
	n := int(header[1])<<16 | int(header[2])<<8 | int(header[3])
	if n > limit {
		return nil, nil, errors.New("hds: frame too long")
	}
	body = make([]byte, n+16)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, nil, err
	}
	return header, body, nil
}

func nonce(n uint64) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint64(b[4:], n)
	return b
}

func (c *Conn) open(header, body []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(c.read[:])
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce(c.readN), body, header)
}

// decodePayload splits a plaintext payload: header length, header
// dictionary, message dictionary.
func (c *Conn) decodePayload(p []byte) (map[string]any, map[string]any, error) {
	if len(p) < 1 || len(p) < 1+int(p[0]) {
		return nil, nil, errShort
	}
	h, _, err := decodeDict(p[1 : 1+int(p[0])])
	if err != nil {
		return nil, nil, err
	}
	m := map[string]any{}
	if rest := p[1+int(p[0]):]; len(rest) > 0 {
		if m, _, err = decodeDict(rest); err != nil {
			return nil, nil, err
		}
	}
	return h, m, nil
}

// Read returns the next message: its header and its body.
func (c *Conn) Read() (map[string]any, map[string]any, error) {
	header, body, err := readFrame(c.nc, maxFrame)
	if err != nil {
		return nil, nil, err
	}
	plain, err := c.open(header, body)
	if err != nil {
		return nil, nil, err
	}
	c.readN++
	return c.decodePayload(plain)
}

// send writes one message.
func (c *Conn) send(header, body map[string]any) error {
	h, err := encodeValue(nil, header)
	if err != nil {
		return err
	}
	if len(h) > 255 {
		return errors.New("hds: header too long")
	}
	payload := append([]byte{byte(len(h))}, h...)
	if payload, err = encodeValue(payload, body); err != nil {
		return err
	}
	if len(payload) > maxFrame {
		return errors.New("hds: message too long")
	}
	fh := []byte{1, byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}
	aead, err := chacha20poly1305.New(c.write[:])
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	frame := aead.Seal(append([]byte(nil), fh...), nonce(c.writeN), payload, fh)
	c.writeN++
	// A hub gone mid-recording would block the write for the TCP
	// retransmission timeout (~15 min), keeping the camera busy.
	_ = c.nc.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = c.nc.Write(frame)
	return err
}

// Respond answers a request with a status (0: success) and a body.
func (c *Conn) Respond(request map[string]any, status int64, body map[string]any) error {
	return c.send(map[string]any{
		"protocol": request["protocol"],
		"response": request["request"],
		"id":       request["id"],
		"status":   status,
	}, body)
}

// Event sends an event.
func (c *Conn) Event(protocol, topic string, body map[string]any) error {
	return c.send(map[string]any{"protocol": protocol, "event": topic}, body)
}

// Close closes the connection.
func (c *Conn) Close() error { return c.nc.Close() }

// RemoteAddr is the controller's address.
func (c *Conn) RemoteAddr() net.Addr { return c.nc.RemoteAddr() }
