// rtptest reads a raw H.264 Annex B file and streams it as RTP to a
// UDP destination, using the same H264Packetizer used by the HomeKit
// camera publisher. No SRTP encryption — for offline validation only.
//
// Usage:
//
//	go run ./cmd/rtptest/ -in /tmp/dump.h264 -dst 127.0.0.1:5000
//
// On the receiving side:
//
//	ffplay -protocol_whitelist file,udp,rtp -i sdp.txt
//
// where sdp.txt is:
//
//	v=0
//	o=- 0 0 IN IP4 127.0.0.1
//	s=Test
//	c=IN IP4 127.0.0.1
//	t=0 0
//	m=video 5000 RTP/AVP 99
//	a=rtpmap:99 H264/90000
//	a=fmtp:99 packetization-mode=1
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/caligone/openqiara/internal/publisher"
)

func main() {
	in := flag.String("in", "/tmp/dump.h264", "input H.264 Annex B file")
	dst := flag.String("dst", "127.0.0.1:5000", "destination UDP host:port")
	fps := flag.Float64("fps", 25.0, "frames per second")
	flag.Parse()

	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("read: %v", err)
	}
	log.Printf("loaded %d bytes from %s", len(data), *in)

	// Split into NAL units (Annex B → NAL bodies).
	nals := splitAnnexB(data)
	log.Printf("found %d NAL units", len(nals))

	// Connect UDP.
	addr, err := net.ResolveUDPAddr("udp4", *dst)
	if err != nil {
		log.Fatalf("resolve: %v", err)
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	log.Printf("sending RTP H.264 to %s", *dst)

	pkter := publisher.NewH264Packetizer(0xCAFEBABE)
	pkter.SetPayloadType(99)

	frameInterval := time.Duration(float64(time.Second) / *fps)
	tickStart := time.Now()
	frameNum := 0

	// Heuristic: a frame ends when we encounter the first SPS/PPS/IDR
	// of the next access unit, OR when we've seen one VCL NAL and the
	// next NAL is also VCL — but that's not reliable. Simplest: treat
	// every IDR or slice as one frame, prepend SPS/PPS as needed.
	for i, nal := range nals {
		if len(nal) == 0 {
			continue
		}
		nalType := nal[0] & 0x1F
		isVCL := nalType == 1 || nalType == 5

		// Use 90 kHz timestamp tied to frame number for deterministic
		// behaviour. iOS clock is 90 kHz so this is the right unit.
		ts90 := uint32(frameNum * 90000 / int(*fps+0.5))
		pkter.SetTimestamp(ts90)

		// Last NAL of frame? heuristic: peek ahead.
		isLast := false
		if isVCL {
			isLast = true
			if i+1 < len(nals) {
				nextType := nals[i+1][0] & 0x1F
				// If next NAL is non-VCL (SEI/SPS/PPS), we're not last yet... but
				// usually SEI come *before* the slice. So if next is also slice/IDR
				// it's a new frame and we ARE last.
				if nextType == 1 || nextType == 5 {
					isLast = true
				}
			}
		}

		pkts := pkter.PacketizeWithMarker(nal, isLast)
		for _, p := range pkts {
			buf, err := p.Marshal()
			if err != nil {
				log.Printf("marshal: %v", err)
				continue
			}
			if _, err := conn.Write(buf); err != nil {
				log.Printf("write: %v", err)
			}
		}

		if isVCL {
			frameNum++
			// Pace frames in real time.
			next := tickStart.Add(time.Duration(frameNum) * frameInterval)
			d := time.Until(next)
			if d > 0 {
				time.Sleep(d)
			}
		}
	}

	log.Printf("sent %d frames in %v", frameNum, time.Since(tickStart))
	fmt.Println("done")
}

// splitAnnexB extracts NAL bodies from an Annex B byte stream.
func splitAnnexB(data []byte) [][]byte {
	var starts []int
	i := 0
	for i+2 < len(data) {
		if data[i] != 0 || data[i+1] != 0 {
			i++
			continue
		}
		if data[i+2] == 1 {
			starts = append(starts, i+3)
			i += 3
			continue
		}
		if i+3 < len(data) && data[i+2] == 0 && data[i+3] == 1 {
			starts = append(starts, i+4)
			i += 4
			continue
		}
		i++
	}
	if len(starts) == 0 {
		return nil
	}
	var nals [][]byte
	for k, ns := range starts {
		var ne int
		if k+1 < len(starts) {
			next := starts[k+1]
			ne = next - 3
			if ne >= 1 && data[ne-1] == 0 {
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
