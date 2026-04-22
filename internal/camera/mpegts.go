// Package camera — MPEG-TS parser.
//
// Reads the MPEG-TS chunks produced by hlcamd, reassembles PES packets,
// and emits H.264 NAL units (Annex B style, start-code delimited) and
// AAC ADTS frames on dedicated channels. Used by the HomeKit camera
// streaming pipeline.
//
// We deliberately do NOT use the PMT/PAT to identify stream PIDs because
// the Qiara hlcamd binary lies about the codec type in the PMT (declares
// HEVC even when --use-h264 is in effect). Instead we autodetect by
// looking at the PES payload start: H.264 NAL units begin with the
// start-code 0x00000001 followed by a NAL header byte whose lower 5
// bits are a valid H.264 NAL type (1..23 for VCL/non-VCL).

package camera

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
)

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47
)

// Sample is one decoded media sample emitted by the parser.
// For video: a single H.264 NAL unit (without the Annex B start code).
// For audio: a single AAC frame (still framed in ADTS).
type Sample struct {
	IsVideo bool
	PTS     int64 // 90 kHz clock for video, 90 kHz also for audio in MPEG-TS
	Data    []byte
}

// MPEGTSParser is a stateful parser that consumes MPEG-TS packets and
// produces media samples. Create one with NewMPEGTSParser, feed it bytes
// via Feed, and consume samples from Samples().
//
// Parser is single-goroutine; protect with a mutex if used from
// multiple producers.
type MPEGTSParser struct {
	logger *slog.Logger

	// Per-PID continuity buffers for PES reassembly.
	pesBufs map[uint16]*pesBuf

	// Auto-detected video and audio PIDs. -1 until first detection.
	videoPID int
	audioPID int

	samples chan Sample
}

type pesBuf struct {
	buf       []byte
	pts       int64
	hasPayload bool
}

// NewMPEGTSParser returns a parser that emits samples on the channel
// returned by Samples(). The channel is buffered (16) to absorb bursts.
// Call Close to release resources.
func NewMPEGTSParser(logger *slog.Logger) *MPEGTSParser {
	if logger == nil {
		logger = slog.Default()
	}
	return &MPEGTSParser{
		logger:   logger,
		pesBufs:  make(map[uint16]*pesBuf),
		videoPID: -1,
		audioPID: -1,
		samples:  make(chan Sample, 16),
	}
}

// Samples returns the channel on which decoded samples are sent.
func (p *MPEGTSParser) Samples() <-chan Sample {
	return p.samples
}

// Flush emits any pending PES packets currently held in continuity
// buffers. Call this between chunks if you want bytes from one chunk to
// be emitted before bytes from the next, or after the last chunk to
// drain the parser. Without Flush, the very last PES of a stream is
// held forever waiting for a continuation that never comes.
func (p *MPEGTSParser) Flush(ctx context.Context) error {
	for pid, buf := range p.pesBufs {
		if buf.hasPayload {
			if err := p.flushPES(ctx, pid, buf); err != nil {
				return err
			}
			buf.hasPayload = false
			buf.buf = buf.buf[:0]
		}
	}
	return nil
}

// Close releases the samples channel. Safe to call once.
func (p *MPEGTSParser) Close() {
	close(p.samples)
}

// Feed processes a chunk of MPEG-TS data. The chunk must contain whole
// 188-byte packets (which is the case for HLS .m4s files produced by
// hlcamd). Returns the number of packets processed and any parse error.
//
// Feed honours ctx for cancellation when sending samples downstream.
func (p *MPEGTSParser) Feed(ctx context.Context, data []byte) (int, error) {
	if len(data)%tsPacketSize != 0 {
		// Hlcamd sometimes writes a partial trailing packet between
		// segment boundaries. Truncate to whole packets.
		data = data[:len(data)-(len(data)%tsPacketSize)]
	}

	processed := 0
	for off := 0; off+tsPacketSize <= len(data); off += tsPacketSize {
		pkt := data[off : off+tsPacketSize]
		if pkt[0] != tsSyncByte {
			// Resync: scan forward for the next 0x47 aligned at 188.
			// Rare for well-formed input.
			next := findResync(data[off+1:])
			if next < 0 {
				return processed, fmt.Errorf("ts: lost sync at offset %d, no resync found", off)
			}
			off += 1 + next - tsPacketSize // -tsPacketSize because the loop will add it back
			continue
		}
		if err := p.processPacket(ctx, pkt); err != nil {
			if errors.Is(err, context.Canceled) {
				return processed, err
			}
			p.logger.Debug("ts: packet error", "error", err)
		}
		processed++
	}
	return processed, nil
}

// findResync scans for the next aligned MPEG-TS sync byte (0x47 followed
// by another 0x47 188 bytes later). Returns the offset within data, or
// -1 if not found.
func findResync(data []byte) int {
	for i := 0; i+tsPacketSize < len(data); i++ {
		if data[i] == tsSyncByte && data[i+tsPacketSize] == tsSyncByte {
			return i
		}
	}
	return -1
}

// processPacket parses a single 188-byte TS packet.
func (p *MPEGTSParser) processPacket(ctx context.Context, pkt []byte) error {
	// TS packet header (4 bytes):
	//   sync_byte                  8 bits = 0x47
	//   transport_error_indicator  1 bit
	//   payload_unit_start         1 bit  ← marks start of new PES
	//   transport_priority         1 bit
	//   PID                        13 bits
	//   transport_scrambling_ctrl  2 bits
	//   adaptation_field_ctrl      2 bits  ← 01=payload, 10=adapt, 11=both
	//   continuity_counter         4 bits

	if pkt[1]&0x80 != 0 {
		// Transport error — discard.
		return nil
	}

	pusi := pkt[1]&0x40 != 0
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	afc := (pkt[3] >> 4) & 0x3

	// Skip null packets (PID 0x1FFF) and PAT/PMT (we don't trust them).
	if pid == 0x1FFF {
		return nil
	}

	// Compute payload offset based on adaptation field.
	payloadStart := 4
	switch afc {
	case 0x0, 0x2:
		// 0x0 = reserved, 0x2 = adaptation only (no payload). Skip.
		return nil
	case 0x3:
		// Adaptation + payload: skip the adaptation field.
		if payloadStart >= len(pkt) {
			return nil
		}
		afLen := int(pkt[4])
		payloadStart = 5 + afLen
		if payloadStart >= len(pkt) {
			return nil
		}
	}
	payload := pkt[payloadStart:]
	if len(payload) == 0 {
		return nil
	}

	// We only care about PES PIDs (audio/video), not PSI tables. Use
	// the PUSI heuristic: when PUSI=1 and the payload starts with the
	// PES start code 0x000001, it's a PES.
	buf, ok := p.pesBufs[pid]
	if pusi {
		// New PES starts here. If we had a previous PES buffered, flush it.
		if ok && buf.hasPayload {
			if err := p.flushPES(ctx, pid, buf); err != nil {
				return err
			}
		}
		// Validate PES start code.
		if len(payload) < 9 || payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
			// Not a PES. Probably PAT/PMT/SDT — ignore the PID.
			delete(p.pesBufs, pid)
			return nil
		}
		streamID := payload[3]
		// Auto-detect video / audio PID by stream ID range.
		// 0xE0..0xEF = video, 0xC0..0xDF = audio.
		if streamID >= 0xE0 && streamID <= 0xEF && p.videoPID < 0 {
			p.videoPID = int(pid)
			p.logger.Info("ts: detected video PID", "pid", fmt.Sprintf("0x%x", pid), "stream_id", fmt.Sprintf("0x%x", streamID))
		} else if streamID >= 0xC0 && streamID <= 0xDF && p.audioPID < 0 {
			p.audioPID = int(pid)
			p.logger.Info("ts: detected audio PID", "pid", fmt.Sprintf("0x%x", pid), "stream_id", fmt.Sprintf("0x%x", streamID))
		}

		// Parse PES header to find PTS and payload start.
		pesPayloadOffset, pts, err := parsePESHeader(payload)
		if err != nil {
			return err
		}

		buf = &pesBuf{
			buf:        append([]byte(nil), payload[pesPayloadOffset:]...),
			pts:        pts,
			hasPayload: true,
		}
		p.pesBufs[pid] = buf
		return nil
	}

	// Continuation packet — append payload to existing buffer.
	if !ok || !buf.hasPayload {
		// We never saw the PUSI for this PID — discard.
		return nil
	}
	buf.buf = append(buf.buf, payload...)
	return nil
}

// parsePESHeader takes a PES packet (starting with 0x000001) and returns
// the offset of the PES payload (NAL units / ADTS frame data) along
// with the PTS in 90 kHz units. Returns -1 PTS if no PTS is present.
func parsePESHeader(pes []byte) (int, int64, error) {
	if len(pes) < 9 {
		return 0, -1, fmt.Errorf("pes: header too short (%d bytes)", len(pes))
	}
	// PES packet header layout:
	//   0..2  start code 00 00 01
	//   3     stream_id
	//   4..5  PES_packet_length (0 = unbounded for video)
	//   6     marker bits + scrambling + priority + alignment + copyright + original
	//   7     PTS_DTS_flags (top 2 bits) + ...
	//   8     PES_header_data_length

	flags := pes[7]
	hdrLen := int(pes[8])
	payloadOffset := 9 + hdrLen
	if payloadOffset > len(pes) {
		return 0, -1, fmt.Errorf("pes: header_data_length=%d exceeds buffer (%d)", hdrLen, len(pes))
	}

	pts := int64(-1)
	// PTS_DTS_flags: bit 7 = PTS present, bit 6 = DTS present.
	if flags&0x80 != 0 {
		// PTS is encoded in 5 bytes starting at offset 9.
		if len(pes) < 14 {
			return 0, -1, fmt.Errorf("pes: pts flag set but buffer too short")
		}
		pts = parsePTS(pes[9:14])
	}
	return payloadOffset, pts, nil
}

// parsePTS decodes the 5-byte MPEG PTS encoding into a 33-bit value
// expressed in 90 kHz units.
func parsePTS(b []byte) int64 {
	// Layout (5 bytes, 40 bits):
	//   0010 PPP1  PPPP PPPP  PPPP PPP1  PPPP PPPP  PPPP PPP1
	// 33 PTS bits split across the bytes.
	pts := int64(b[0]&0x0E) << 29
	pts |= int64(b[1]) << 22
	pts |= int64(b[2]&0xFE) << 14
	pts |= int64(b[3]) << 7
	pts |= int64(b[4]) >> 1
	return pts
}

// flushPES finalises a complete PES packet for the given PID. For video
// PIDs we split into NAL units; for audio PIDs we emit the payload as
// one ADTS frame (or several — ADTS framing means iOS-side decoder will
// re-frame).
func (p *MPEGTSParser) flushPES(ctx context.Context, pid uint16, buf *pesBuf) error {
	switch int(pid) {
	case p.videoPID:
		return p.emitVideo(ctx, buf)
	case p.audioPID:
		return p.emitAudio(ctx, buf)
	}
	return nil
}

// emitVideo splits the buffered PES payload into H.264 NAL units and
// sends each on the samples channel. Annex B start codes (0x000001 or
// 0x00000001) delimit NAL units.
func (p *MPEGTSParser) emitVideo(ctx context.Context, buf *pesBuf) error {
	nals := splitNALUnits(buf.buf)
	for _, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		select {
		case p.samples <- Sample{IsVideo: true, PTS: buf.pts, Data: nal}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// emitAudio sends the AAC payload as a single sample (containing ADTS
// frames). HomeKit RTP audio packetization will frame as needed.
func (p *MPEGTSParser) emitAudio(ctx context.Context, buf *pesBuf) error {
	if len(buf.buf) == 0 {
		return nil
	}
	select {
	case p.samples <- Sample{IsVideo: false, PTS: buf.pts, Data: append([]byte(nil), buf.buf...)}:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// splitNALUnits splits an Annex B byte stream into individual NAL units,
// stripping the start codes. Returns each NAL unit's body (the first
// byte is the NAL header).
//
// Annex B format: each NAL unit is preceded by either 0x000001 (3-byte
// start code) or 0x00000001 (4-byte start code). The end of a NAL unit
// is the start code of the next NAL unit, or end of buffer.
func splitNALUnits(data []byte) [][]byte {
	// Find every start code position (returns offset of the first byte
	// AFTER the start code, i.e. the NAL header byte).
	var starts []int
	i := 0
	for i+2 < len(data) {
		if data[i] != 0x00 || data[i+1] != 0x00 {
			i++
			continue
		}
		// data[i..i+1] = 00 00
		if data[i+2] == 0x01 {
			// 3-byte start code 00 00 01 → NAL begins at i+3
			starts = append(starts, i+3)
			i += 3
			continue
		}
		if i+3 < len(data) && data[i+2] == 0x00 && data[i+3] == 0x01 {
			// 4-byte start code 00 00 00 01 → NAL begins at i+4
			starts = append(starts, i+4)
			i += 4
			continue
		}
		i++
	}

	if len(starts) == 0 {
		return nil
	}

	// Build NAL units: each NAL goes from starts[k] to (starts[k+1] - sclen),
	// where sclen is 3 or 4 depending on what precedes starts[k+1].
	var nals [][]byte
	for k, ns := range starts {
		var ne int
		if k+1 < len(starts) {
			next := starts[k+1]
			// Determine how many bytes the next start code consumed
			// (3 or 4) by looking back from `next`.
			ne = next - 3
			if ne >= 1 && data[ne-1] == 0x00 {
				// 4-byte start code (00 00 00 01)
				ne--
			}
		} else {
			ne = len(data)
		}
		if ne > ns && ne <= len(data) {
			nals = append(nals, data[ns:ne])
		}
	}
	return nals
}

// Helper for testing — exposed at package level for tests in another file.
var _ = binary.BigEndian
