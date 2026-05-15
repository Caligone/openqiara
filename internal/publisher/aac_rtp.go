// Package publisher — AAC over RTP packetizer.
//
// Implements RFC 3640 "mpeg4-generic" mode for streaming AAC frames
// over RTP, with one access unit per packet (the simplest profile).
//
// CURRENTLY UNUSED. Empirical testing showed that iOS HomeKit silently
// rejects raw AAC-LC frames sent in this format — the iOS AudioToolbox
// decoder seems to strictly require AAC-ELD when the audio config TLV
// announces AAC-ELD. We don't have a way to transcode AAC-LC → AAC-ELD
// in pure Go (no encoder exists). The functions below are kept as
// groundwork for a future CGo libfdk-aac integration that would
// produce real AAC-ELD frames; the RFC 3640 packetization layer here
// is reusable as-is.

package publisher

import (
	"github.com/pion/rtp"
)

// stripADTS removes the 7-byte ADTS header from an AAC-LC frame
// extracted from MPEG-TS. Returns the raw AAC payload (the part RFC
// 3640 calls the "access unit"). If the input doesn't start with an
// ADTS sync word (0xFFF), it's returned unchanged.
//
//nolint:unused // groundwork for future AAC-ELD transcoding via libfdk-aac
//
// ADTS layout (7 bytes):
//
//	0xFF 0xF1 ...   sync (12 bits) + ID + layer + protection_absent
//	... profile, sample rate, channels (12 bits across two bytes)
//	frame_length (13 bits) — TOTAL bytes including the 7-byte header
//
// We don't need to parse the profile/rate/channels here; the HomeKit
// session config tells iOS what to expect. We only need to find the
// raw AAC bytes.
func stripADTS(frame []byte) []byte {
	if len(frame) < 7 {
		return frame
	}
	// ADTS sync word: 0xFFF in the top 12 bits.
	if frame[0] != 0xFF || (frame[1]&0xF0) != 0xF0 {
		return frame
	}
	// Frame length (13 bits) starts at bit 30 (within bytes 3-5).
	frameLen := (int(frame[3]&0x03) << 11) | (int(frame[4]) << 3) | (int(frame[5]&0xE0) >> 5)
	if frameLen < 7 || frameLen > len(frame) {
		return frame
	}
	return frame[7:frameLen]
}

// splitADTSFrames takes a buffer that may contain MULTIPLE concatenated
// ADTS frames (which is how MPEG-TS audio PES often packs them) and
// returns each raw AAC payload separately, with ADTS headers stripped.
//
//nolint:unused // groundwork for future AAC-ELD transcoding via libfdk-aac
func splitADTSFrames(buf []byte) [][]byte {
	var out [][]byte
	for off := 0; off+7 <= len(buf); {
		if buf[off] != 0xFF || (buf[off+1]&0xF0) != 0xF0 {
			off++
			continue
		}
		frameLen := (int(buf[off+3]&0x03) << 11) | (int(buf[off+4]) << 3) | (int(buf[off+5]&0xE0) >> 5)
		if frameLen < 7 || off+frameLen > len(buf) {
			break
		}
		out = append(out, buf[off+7:off+frameLen])
		off += frameLen
	}
	return out
}

// packetizeAAC builds one RTP packet containing a single AAC access
// unit per RFC 3640 (mpeg4-generic mode). The AU-headers-length field
// is fixed at 16 bits (one header), and the AU header itself encodes
// the AU size in 13 bits and an index of 0 in the low 3 bits.
//
// Returned packet has Marker=true (every audio frame is a complete
// access unit, RFC 3550 audio convention).
func packetizeAAC(aacRaw []byte, ssrc uint32, seq uint16, ts uint32, payloadType uint8) *rtp.Packet {
	if len(aacRaw) == 0 {
		return nil
	}
	// AU-headers-length: 16 bits, value = 16 (one header of 16 bits).
	// AU header: 13-bit AU size + 3-bit AU index (= 0).
	auSize := uint16(len(aacRaw))
	header := make([]byte, 4+len(aacRaw))
	header[0] = 0x00
	header[1] = 0x10 // AU-headers-length = 16 bits
	header[2] = byte(auSize >> 5)
	header[3] = byte(auSize<<3) & 0xF8 // index = 0 in low 3 bits
	copy(header[4:], aacRaw)

	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         true,
		},
		Payload: header,
	}
}
