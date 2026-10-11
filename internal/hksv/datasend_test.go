package hksv

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/mediahub"
)

func newHub(t *testing.T) *mediahub.Hub {
	t.Helper()
	return mediahub.New("fake", fakeCamera(2*time.Millisecond), nil, quiet)
}

// dial connects a controller to ds, as iOS does, hello included.
func dial(t *testing.T, ds *DataStream) *controller {
	t.Helper()
	var shared [32]byte
	_, _ = rand.Read(shared[:])
	controllerSalt := make([]byte, 32)
	_, _ = rand.Read(controllerSalt)
	resp, err := ds.Setup(encode(item{0x01, u8(0)}, item{0x02, u8(0)}, item{0x03, controllerSalt}), shared)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := decode(resp)
	params, _ := decode(first(items, 0x02))
	port := binary.LittleEndian.Uint16(first(params, 0x01))
	salt := append(append([]byte(nil), controllerSalt...), first(items, 0x03)...)
	c := &controller{t: t}
	_ = derive(shared, salt, "HDS-Read-Encryption-Key", &c.read)
	_ = derive(shared, salt, "HDS-Write-Encryption-Key", &c.write)
	if c.nc, err = net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(int(port))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.nc.Close() })
	c.send(map[string]any{"protocol": "control", "request": "hello", "id": int64(1)}, map[string]any{})
	if h, _ := c.recv(); h["response"] != "hello" {
		t.Fatalf("hello response %#v", h)
	}
	return c
}

func (c *controller) open(id, stream int64) (map[string]any, map[string]any) {
	c.send(map[string]any{"protocol": "dataSend", "request": "open", "id": id},
		map[string]any{"target": "controller", "type": "ipcamera.recording", "streamId": stream, "reason": "motion"})
	return c.recv()
}

func TestRecordingOverDataStream(t *testing.T) {
	r := NewRecorder(1000, quiet)
	r.SetHub(newHub(t))
	open := func() (RecordFunc, int) {
		r.mu.Lock()
		on := r.stop != nil
		r.mu.Unlock()
		if !on {
			return nil, ReasonNotAllowed
		}
		return func(ctx context.Context, emit func([]byte, bool) error) error {
			return r.Record(ctx, Options{FragmentMs: 1000}, emit)
		}, 0
	}
	ds := NewDataStream(NewDataSend(open, quiet).Serve, quiet)
	defer ds.Close()
	c := dial(t, ds)

	// Recording off: refused, not allowed.
	if h, b := c.open(2, 1); h["status"] != int64(statusProtocolSpecific) || b["status"] != int64(ReasonNotAllowed) {
		t.Fatalf("open while off: %#v %#v", h, b)
	}
	// Unknown requests get an answer.
	c.send(map[string]any{"protocol": "foo", "request": "bar", "id": int64(3)}, map[string]any{})
	if h, _ := c.recv(); h["response"] != "bar" || h["status"] != int64(statusMissingProtocol) {
		t.Fatalf("unknown request: %#v", h)
	}

	r.SetActive(true)
	defer r.SetActive(false)
	waitPrebuffer(t, r)
	if h, b := c.open(4, 7); h["response"] != "open" || h["id"] != int64(4) || h["status"] != int64(0) || b["status"] != int64(0) {
		t.Fatalf("open: %#v %#v", h, b)
	}

	var media [][]byte
	for seq := int64(1); seq <= 3; seq++ {
		h, b := c.recv()
		if h["protocol"] != "dataSend" || h["event"] != "data" || b["streamId"] != int64(7) {
			t.Fatalf("data event %#v %#v", h, b)
		}
		packets := b["packets"].([]any)
		p := packets[0].(map[string]any)
		meta := p["metadata"].(map[string]any)
		data := p["data"].([]byte)
		kind := "mediaFragment"
		if seq == 1 {
			kind = "mediaInitialization"
		}
		if meta["dataType"] != kind || meta["dataSequenceNumber"] != seq ||
			meta["dataChunkSequenceNumber"] != int64(1) || meta["isLastDataChunk"] != true ||
			meta["dataTotalSize"] != int64(len(data)) || b["endOfStream"] != false {
			t.Fatalf("packet %d: %#v, endOfStream %#v", seq, meta, b["endOfStream"])
		}
		media = append(media, data)
	}
	_, parts := parse(t, media[0], media[1:]...)
	if len(parts) != 2 || parts[0].SequenceNumber != 1 || parts[1].SequenceNumber != 2 {
		t.Fatalf("fragments %d", len(parts))
	}

	// A second recording while one runs: busy.
	if h, b := c.open(5, 8); h["status"] != int64(statusProtocolSpecific) || b["status"] != int64(ReasonBusy) {
		t.Fatalf("second open: %#v %#v", h, b)
	}

	// The hub closes it: the camera stops sending, and may record again.
	c.send(map[string]any{"protocol": "dataSend", "event": "close"}, map[string]any{"streamId": int64(7), "reason": int64(ReasonNormal)})
	deadline := time.Now().Add(2 * time.Second)
	for id := int64(6); ; id++ {
		c.send(map[string]any{"protocol": "dataSend", "request": "open", "id": id},
			map[string]any{"target": "controller", "type": "ipcamera.recording", "streamId": id})
		h, b := c.recv()
		for h["response"] != "open" { // fragments already on their way
			h, b = c.recv()
		}
		if b["status"] == int64(0) {
			break
		}
		if b["status"] != int64(ReasonBusy) || time.Now().After(deadline) {
			t.Fatalf("open after close: %#v", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestChunks(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	var k1, k2 [32]byte
	_, _ = rand.Read(k1[:])
	_, _ = rand.Read(k2[:])
	conn := &Conn{nc: a, read: k1, write: k2}
	c := &controller{t: t, nc: b, read: k2, write: k1}

	data := make([]byte, 2*maxChunk+10)
	go func() { _ = sendPacket(conn, int64(3), packetFragment, 2, data, true) }()
	for i, size := range []int{maxChunk, maxChunk, 10} {
		_, body := c.recv()
		meta := body["packets"].([]any)[0].(map[string]any)["metadata"].(map[string]any)
		chunk := body["packets"].([]any)[0].(map[string]any)["data"].([]byte)
		last := i == 2
		if len(chunk) != size || meta["dataChunkSequenceNumber"] != int64(i+1) || meta["isLastDataChunk"] != last ||
			meta["dataSequenceNumber"] != int64(2) {
			t.Fatalf("chunk %d: %d bytes, %#v", i, len(chunk), meta)
		}
		if _, ok := meta["dataTotalSize"]; ok != (i == 0) {
			t.Fatalf("chunk %d: dataTotalSize %#v", i, meta["dataTotalSize"])
		}
		if eos, ok := body["endOfStream"]; ok != last || (last && eos != true) {
			t.Fatalf("chunk %d: endOfStream %#v", i, eos)
		}
	}
}

func TestEndOfStreamAcked(t *testing.T) {
	init, frag := []byte("init"), []byte("fragment")
	opened := make(chan struct{}, 2)
	open := func() (RecordFunc, int) {
		return func(ctx context.Context, emit func([]byte, bool) error) error {
			opened <- struct{}{}
			if err := emit(init, false); err != nil {
				return err
			}
			return emit(frag, true)
		}, 0
	}
	ds := NewDataStream(NewDataSend(open, quiet).Serve, quiet)
	defer ds.Close()
	c := dial(t, ds)
	c.open(2, 1)
	c.recv()
	if _, b := c.recv(); b["endOfStream"] != true {
		t.Fatalf("last packet %#v", b)
	}
	c.send(map[string]any{"protocol": "dataSend", "event": "ack"}, map[string]any{"streamId": int64(1), "endOfStream": true})
	deadline := time.Now().Add(2 * time.Second)
	for id := int64(3); ; id++ {
		if _, b := c.open(id, id); b["status"] == int64(0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the acked recording still runs")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Once the hub closes the stream, nothing more goes out for it.
func TestNoDataAfterClose(t *testing.T) {
	late := make(chan error, 1)
	open := func() (RecordFunc, int) {
		return func(ctx context.Context, emit func([]byte, bool) error) error {
			<-ctx.Done()
			err := emit([]byte("fragment"), false)
			late <- err
			return err
		}, 0
	}
	ds := NewDataStream(NewDataSend(open, quiet).Serve, quiet)
	defer ds.Close()
	c := dial(t, ds)
	c.open(2, 1)
	c.send(map[string]any{"protocol": "dataSend", "event": "close"}, map[string]any{"streamId": int64(1), "reason": int64(0)})
	select {
	case err := <-late:
		if err == nil {
			t.Fatal("data sent after the hub closed the stream")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("recording not cancelled")
	}
}
