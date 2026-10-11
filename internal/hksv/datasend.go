package hksv

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// The recordings travel on the data stream's dataSend protocol
// (HAP-NodeJS RecordingManagement.ts, CameraRecordingStream): the home hub
// requests dataSend/open (target controller, type ipcamera.recording, a
// streamId), the camera answers, then sends dataSend/data events: the init
// segment, then fragments, chunked. The hub ends the stream with a
// dataSend/close event; the camera ends it by flagging endOfStream, which
// the hub acknowledges with dataSend/ack.

// HDS statuses, in a response's header (HAP-NodeJS DataStreamServer.ts
// HDSStatus).
const (
	statusSuccess          = 0
	statusMissingProtocol  = 5
	statusProtocolSpecific = 6
)

// Reasons, in a dataSend response or close (HAP-NodeJS
// HDSProtocolSpecificErrorReason).
const (
	ReasonNormal        = 0
	ReasonNotAllowed    = 1
	ReasonBusy          = 2
	ReasonCancelled     = 3
	ReasonUnsupported   = 4
	ReasonUnexpected    = 5
	ReasonTimeout       = 6
	ReasonBadData       = 7
	ReasonProtocolError = 8
	ReasonInvalidConfig = 9
)

const (
	protocolDataSend = "dataSend"
	recordingType    = "ipcamera.recording"
	packetInit       = "mediaInitialization"
	packetFragment   = "mediaFragment"
	maxChunk         = 0x40000 // HAP-NodeJS: 256 KiB per data event
	// closeTimeout is how long the hub has to ack the end of a stream
	// before the camera closes it (HAP-NodeJS kickOffCloseTimeout).
	closeTimeout = 12 * time.Second
)

// RecordFunc streams one recording, as Recorder.Record does.
type RecordFunc func(ctx context.Context, emit func(data []byte, last bool) error) error

// DataSend serves recordings on data stream connections, one at a time.
type DataSend struct {
	log *slog.Logger
	// Open says whether a recording may start: a reason (ReasonNotAllowed,
	// ReasonInvalidConfig...) to refuse it, or the recording.
	open func() (RecordFunc, int)
	busy atomic.Bool
}

// NewDataSend returns the dataSend server; open is asked at each request.
func NewDataSend(open func() (RecordFunc, int), logger *slog.Logger) *DataSend {
	return &DataSend{open: open, log: logger}
}

// stream is one recording on a connection.
type stream struct {
	id     any // the controller's streamId, echoed as it came
	cancel context.CancelFunc
	done   chan struct{}
	ended  chan struct{} // closed when the hub acked or closed it
	once   sync.Once
}

func (s *stream) end() { s.once.Do(func() { close(s.ended) }) }

// Serve handles a connection's messages until it closes.
func (d *DataSend) Serve(c *Conn) {
	var cur *stream
	defer func() {
		if cur != nil {
			cur.end()
			cur.cancel()
			_ = c.Close() // unblocks a write in progress
			<-cur.done
		}
	}()
	for {
		h, m, err := c.Read()
		if err != nil {
			d.log.Info("hksv: data stream closed", "error", err)
			return
		}
		switch {
		case h["protocol"] == protocolDataSend && h["request"] == "open":
			if s := d.openStream(c, h, m); s != nil {
				cur = s
			}
		case h["protocol"] == protocolDataSend && (h["event"] == "close" || h["event"] == "ack"):
			d.log.Info("hksv: recording "+h["event"].(string), "stream", m["streamId"], "reason", m["reason"], "endOfStream", m["endOfStream"])
			if cur != nil && m["streamId"] == cur.id {
				cur.end()
				cur.cancel()
			}
		case h["request"] != nil:
			// HAP-NodeJS leaves these unanswered; a response at least
			// spares the hub its wait.
			d.log.Info("hksv: unknown data stream request", "header", h)
			_ = c.Respond(h, statusMissingProtocol, map[string]any{})
		default:
			d.log.Debug("hksv: data stream event ignored", "header", h)
		}
	}
}

// openStream answers a dataSend/open, and starts the recording if it may.
func (d *DataSend) openStream(c *Conn, h, m map[string]any) *stream {
	refuse := func(reason int) *stream {
		d.log.Info("hksv: recording refused", "reason", reason, "request", m)
		_ = c.Respond(h, statusProtocolSpecific, map[string]any{"status": int64(reason)})
		return nil
	}
	if m["target"] != "controller" || m["type"] != recordingType {
		return refuse(ReasonUnexpected)
	}
	record, reason := d.open()
	if record == nil {
		return refuse(reason)
	}
	if !d.busy.CompareAndSwap(false, true) {
		return refuse(ReasonBusy)
	}
	if err := c.Respond(h, statusSuccess, map[string]any{"status": int64(statusSuccess)}); err != nil {
		d.busy.Store(false)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &stream{id: m["streamId"], cancel: cancel, done: make(chan struct{}), ended: make(chan struct{})}
	d.log.Info("hksv: recording", "stream", s.id, "reason", m["reason"])
	go func() {
		defer close(s.done)
		defer d.busy.Store(false)
		defer cancel()
		d.send(ctx, c, s, record)
	}()
	return s
}

// send streams the recording as data events, until it ends or the hub
// closes it.
func (d *DataSend) send(ctx context.Context, c *Conn, s *stream, record RecordFunc) {
	seq := int64(1) // the init segment is 1, fragments follow
	err := record(ctx, func(data []byte, last bool) error {
		if err := ctx.Err(); err != nil {
			return err // the hub closed the stream: nothing more for it
		}
		kind := packetFragment
		if seq == 1 {
			kind = packetInit
		}
		if err := sendPacket(c, s.id, kind, seq, data, last); err != nil {
			return err
		}
		seq++
		return nil
	})
	switch {
	case ctx.Err() != nil:
		return // the hub closed it, or the connection went
	case err == nil:
		// endOfStream went out: the hub acks, else it is closed for it.
		select {
		case <-s.ended:
		case <-time.After(closeTimeout):
			d.closeStream(c, s, ReasonCancelled)
		}
	default:
		d.log.Warn("hksv: recording failed", "stream", s.id, "error", err)
		d.closeStream(c, s, ReasonUnexpected)
	}
}

// close ends a stream from the camera's side.
func (d *DataSend) closeStream(c *Conn, s *stream, reason int) {
	_ = c.Event(protocolDataSend, "close", map[string]any{"streamId": s.id, "reason": int64(reason)})
}

// sendPacket sends one init segment or fragment in chunks of at most
// maxChunk, numbered from 1, the first giving the total size; the last
// chunk says whether the stream ends (HAP-NodeJS _startStreaming).
func sendPacket(c *Conn, id any, kind string, seq int64, data []byte, last bool) error {
	if len(data) == 0 {
		return errors.New("hksv: empty packet")
	}
	for off, chunk := 0, int64(1); off < len(data); chunk++ {
		n := min(maxChunk, len(data)-off)
		meta := map[string]any{
			"dataType":                kind,
			"dataSequenceNumber":      seq,
			"dataChunkSequenceNumber": chunk,
			"isLastDataChunk":         off+n == len(data),
		}
		if chunk == 1 {
			meta["dataTotalSize"] = int64(len(data))
		}
		body := map[string]any{
			"streamId": id,
			"packets":  []any{map[string]any{"data": data[off : off+n], "metadata": meta}},
		}
		if off+n == len(data) {
			body["endOfStream"] = last
		}
		if err := c.Event(protocolDataSend, "data", body); err != nil {
			return err
		}
		off += n
	}
	return nil
}
