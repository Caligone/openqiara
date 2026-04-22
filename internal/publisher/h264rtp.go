// Package publisher — H.264 over RTP packetizer.
//
// Implements RFC 6184 (RTP Payload Format for H.264 Video) with the
// minimal subset HomeKit needs:
//   - Single NAL unit packet (one NAL fits in MTU)
//   - FU-A fragmentation (one NAL split across several RTP packets)
//
// We do NOT implement STAP-A (aggregating several small NAL into one
// RTP packet) because HomeKit doesn't require it and it complicates the
// packetizer for marginal gain on a 1080p stream.
//
// SPS and PPS NAL units are cached so they can be re-emitted before
// each IDR (key frame), which is what iOS expects on stream resume.

package publisher

import (
	"sync"
	"time"

	"github.com/pion/rtp"
)

// H264NALType is the 5-bit NAL unit type from the NAL header.
type H264NALType uint8

const (
	H264NALSlice    H264NALType = 1
	H264NALDPA      H264NALType = 2
	H264NALDPB      H264NALType = 3
	H264NALDPC      H264NALType = 4
	H264NALIDR      H264NALType = 5
	H264NALSEI      H264NALType = 6
	H264NALSPS      H264NALType = 7
	H264NALPPS      H264NALType = 8
	H264NALAUD      H264NALType = 9
	H264NALEndSeq   H264NALType = 10
	H264NALEndStrm  H264NALType = 11
	H264NALFiller   H264NALType = 12
)

// h264RTPPayloadType is the dynamic RTP payload type assigned by HomeKit
// in the SelectedRTPStreamConfiguration TLV. We default to 99 (matches
// what we observed in the iOS Home logs) but the actual value comes
// from the HAP setup.
const h264RTPPayloadType = 99

// rtpMTU is the maximum size of an RTP payload (excluding the 12-byte
// RTP header). HomeKit recommends ~1378 bytes to fit a single UDP
// datagram under 1500-byte Ethernet MTU after IP+UDP+SRTP overhead.
const rtpMTU = 1378

// H264Packetizer turns a stream of H.264 NAL units into RTP packets
// ready to be encrypted by SRTP and sent to iOS. One Packetizer per
// streaming session.
//
// Not safe for concurrent use — drive from a single goroutine.
type H264Packetizer struct {
	// SSRC of the video RTP stream — must match the SSRC we announced
	// to iOS in the SetupEndpointsResponse. Set at construction.
	ssrc uint32

	// payloadType is the RTP payload type negotiated by HomeKit. We use
	// h264RTPPayloadType (99) as default but it can be overridden via
	// the constructor.
	payloadType uint8

	// Sequence number — incremented for every packet. Wraps at 16 bits.
	seq uint16

	// 90 kHz monotonic clock for RTP timestamps. We use the wall clock
	// instead of the PTS from the source MPEG-TS to avoid jitter from
	// hlcamd's clock. iOS doesn't care as long as it's monotonic.
	// (Set externally via NextTimestamp.)
	timestamp uint32

	// Cached SPS and PPS for re-emission on every IDR. Some encoders
	// only emit SPS/PPS once at the start of the stream; iOS will fail
	// to decode the IDR if SPS/PPS aren't in band, so we keep a copy.
	mu  sync.Mutex
	sps []byte
	pps []byte
}

// NewH264Packetizer returns a packetizer for a single video session.
// The starting sequence number is randomised so iOS doesn't see two
// streams from the same SSRC restart from 0 across reconfigures.
func NewH264Packetizer(ssrc uint32) *H264Packetizer {
	return &H264Packetizer{
		ssrc:        ssrc,
		payloadType: h264RTPPayloadType,
		seq:         uint16(time.Now().UnixNano() & 0xFFFF),
	}
}

// SetPayloadType overrides the RTP payload type. Call before Packetize.
func (p *H264Packetizer) SetPayloadType(pt uint8) {
	p.payloadType = pt
}

// SetTimestamp updates the RTP timestamp for the next packet. Call once
// per video frame with a 90 kHz monotonic value (e.g. wallClockMs * 90).
func (p *H264Packetizer) SetTimestamp(ts uint32) {
	p.timestamp = ts
}

// Packetize converts a single H.264 NAL unit (Annex B body, without
// the start code) into one or more RTP packets. The marker bit on the
// last packet is always set; use PacketizeWithMarker for finer control.
//
// SPS (type 7) and PPS (type 8) units are cached and re-emitted before
// every IDR (type 5) automatically.
func (p *H264Packetizer) Packetize(nal []byte) []*rtp.Packet {
	return p.PacketizeWithMarker(nal, true)
}

// PacketizeWithMarker is like Packetize but lets the caller decide if
// the marker bit (RFC 6184: end of access unit) should be set on the
// last RTP packet of this NAL. Pass true when this NAL is the last
// VCL NAL of a video frame, false otherwise.
//
// The SPS/PPS that we prepend before each IDR never carry the marker
// bit even when isLastOfAU is true — only the IDR slice itself does.
func (p *H264Packetizer) PacketizeWithMarker(nal []byte, isLastOfAU bool) []*rtp.Packet {
	if len(nal) == 0 {
		return nil
	}

	nalType := H264NALType(nal[0] & 0x1F)

	// Cache SPS and PPS for IDR re-emission.
	switch nalType {
	case H264NALSPS:
		p.mu.Lock()
		p.sps = append(p.sps[:0], nal...)
		p.mu.Unlock()
	case H264NALPPS:
		p.mu.Lock()
		p.pps = append(p.pps[:0], nal...)
		p.mu.Unlock()
	}

	var out []*rtp.Packet

	// Before an IDR, prepend SPS and PPS so iOS can decode the keyframe
	// even if it tunes in mid-stream or after a packet loss burst.
	if nalType == H264NALIDR {
		p.mu.Lock()
		if len(p.sps) > 0 {
			out = append(out, p.packetizeNAL(p.sps, false)...)
		}
		if len(p.pps) > 0 {
			out = append(out, p.packetizeNAL(p.pps, false)...)
		}
		p.mu.Unlock()
	}

	out = append(out, p.packetizeNAL(nal, isLastOfAU)...)
	return out
}

// packetizeNAL packetizes one NAL unit (without start code). If the NAL
// fits within rtpMTU, emit a single-NAL packet. Otherwise fragment into
// FU-A packets per RFC 6184 §5.8. The marker bit is set on the LAST
// packet of an access unit (the last fragment of the last NAL of a
// frame) — we approximate by setting it on every "main" NAL (slices),
// not on prepended SPS/PPS.
func (p *H264Packetizer) packetizeNAL(nal []byte, setMarker bool) []*rtp.Packet {
	if len(nal) == 0 {
		return nil
	}

	if len(nal) <= rtpMTU {
		// Single NAL unit packet.
		pkt := p.makePacket(nal, setMarker)
		return []*rtp.Packet{pkt}
	}

	// FU-A fragmentation. Split the NAL body (without the header byte)
	// into chunks of (rtpMTU - 2) and prefix each fragment with the
	// FU indicator (1 byte) + FU header (1 byte).
	nalHeader := nal[0]
	nalBody := nal[1:]

	// FU indicator: same NRI as the original NAL header, but type = 28 (FU-A).
	fuIndicator := (nalHeader & 0xE0) | 28

	const fuPayloadMTU = rtpMTU - 2

	var pkts []*rtp.Packet
	for off := 0; off < len(nalBody); off += fuPayloadMTU {
		end := off + fuPayloadMTU
		if end > len(nalBody) {
			end = len(nalBody)
		}
		isFirst := off == 0
		isLast := end == len(nalBody)

		// FU header: S (start, 1 bit) + E (end, 1 bit) + R (reserved, 1 bit) + type (5 bits).
		fuHeader := nalHeader & 0x1F
		if isFirst {
			fuHeader |= 0x80
		}
		if isLast {
			fuHeader |= 0x40
		}

		payload := make([]byte, 2+(end-off))
		payload[0] = fuIndicator
		payload[1] = fuHeader
		copy(payload[2:], nalBody[off:end])

		// Marker only on the LAST fragment of the access unit.
		marker := setMarker && isLast
		pkts = append(pkts, p.makePacket(payload, marker))
	}
	return pkts
}

// makePacket builds and returns one RTP packet with the given payload.
// The sequence number is incremented after each packet.
func (p *H264Packetizer) makePacket(payload []byte, marker bool) *rtp.Packet {
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    p.payloadType,
			SequenceNumber: p.seq,
			Timestamp:      p.timestamp,
			SSRC:           p.ssrc,
			Marker:         marker,
		},
		Payload: payload,
	}
	p.seq++
	return pkt
}
