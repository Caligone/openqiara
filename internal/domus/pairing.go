package domus

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// Pairing opcodes on the CTRL channel.
const (
	opStartPairing  = 0x15
	opStopPairing   = 0x16
	opBeacon        = 0x17
	opPairRequest   = 0x1a
	opPairChallenge = 0x1f
	opPairConfirm   = 0x1c
	opPairResult    = 0x1e
)

// PairingResult contains the outcome of a successful pairing.
type PairingResult struct {
	DeviceUID  [8]byte
	Model      string
	VendorName string
	Address    byte
}

// PairSensor runs the full DomusRF pairing via charmux, without fbxhome.
// Protocol captured from fbxhome tcpdump (2026-03-31).
//
// CTRL handshake:
//  1. Send 0x15 (start pairing, flags=0x02)
//  2. Wait for beacon → match vendor key → send 0x1a (pair request)
//  3. Wait for challenge (0x1f) → send 0x1c (confirm)
//  4. Wait for result (0x1e) + stop (0x16)
//
// PKT config (after CTRL):
//  5. Wait for sensor heartbeat on PKT (~1.5s after CTRL)
//  6. Respond with bytecode + config managed frames
func PairSensor(ctx context.Context, mux *charmux.Client, keys []VendorKey, nextAddr byte, logger *slog.Logger) (*PairingResult, error) {
	events := mux.Events()

	// Step 1: Reproduce exact fbxhome pairing sequence (from our_pairing.pcap).
	// fbxhome does: GetInfo → GetNet → 0x15 → wait for 0x16 → 0x15 again → wait beacon.
	// The double 0x15 may be required for MCU NVM persistence.
	ctxInit, cancelInit := context.WithTimeout(ctx, 3*time.Second)
	if resp, err := mux.GetInfo(ctxInit); err == nil {
		logger.Info("pairing: GetInfo", "state", resp.State)
	}
	cancelInit()
	ctxNet, cancelNet := context.WithTimeout(ctx, 3*time.Second)
	mux.GetNet(ctxNet)
	cancelNet()

	// First 0x15 — MCU may respond with 0x16 (stop) then we resend
	cmd := make([]byte, 18)
	cmd[0] = opStartPairing
	cmd[1] = nextAddr
	cmd[5] = nextAddr
	logger.Info("pairing: sending START_PAIRING (0x15) [1st]", "next_addr", nextAddr)
	mux.SendRawCTRL(cmd)

	// Send watchdog between the two 0x15 (captured from fbxhome)
	mux.SendWatchdog()

	// Wait briefly for 0x16 spontaneous stop from MCU, then resend 0x15
	time.Sleep(500 * time.Millisecond)
	logger.Info("pairing: sending START_PAIRING (0x15) [2nd]", "next_addr", nextAddr)
	mux.SendRawCTRL(cmd)

	logger.Info("pairing: waiting for beacon (reset sensor now)...")
	deadline := time.After(90 * time.Second)

	var model string
	var uid [8]byte
	var matchedKey *VendorKey
	var address byte

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			mux.SendRawCTRL([]byte{opStopPairing})
			return nil, fmt.Errorf("pairing: timeout")
		case evt, ok := <-events:
			if !ok {
				return nil, fmt.Errorf("pairing: events closed")
			}
			if len(evt.Data) == 0 {
				continue
			}
			op := evt.Data[0] & 0x7F

			// Beacon (0x17)
			if op == opBeacon && len(evt.Data) >= 31 && matchedKey == nil {
				model = string(trimNull(evt.Data[15:31]))
				copy(uid[:], evt.Data[7:15])
				key, ok := MatchBeacon(evt.Data, keys)
				if !ok {
					logger.Warn("pairing: no matching vendor key")
					continue
				}
				matchedKey = &key
				logger.Info("pairing: beacon received", "model", model, "vendor", key.Name)

				// Send pair request (0x1a)
				accept := make([]byte, 57)
				accept[0] = opPairRequest
				copy(accept[1:9], uid[:])
				copy(accept[9:41], key.Key[:])
				logger.Info("pairing: sending pair request (0x1a)")
				mux.SendRawCTRL(accept)
				continue
			}

			// Challenge (0x1f)
			if op == opPairChallenge && matchedKey != nil {
				logger.Info("pairing: challenge received (0x1f)")

				confirm := make([]byte, 9)
				confirm[0] = opPairConfirm
				copy(confirm[1:9], uid[:])
				logger.Info("pairing: sending confirmation (0x1c)")
				mux.SendRawCTRL(confirm)
				continue
			}

			// Result (0x1e)
			if op == opPairResult && matchedKey != nil {
				if len(evt.Data) >= 10 {
					address = evt.Data[9]
				}
				logger.Info("pairing: result received (0x1e)", "address", address)
				continue
			}

			// Stop (0x16) — after result, MCU signals handshake done
			if op == opStopPairing && matchedKey != nil {
				logger.Info("pairing: stop received (0x16)")
				// Echo the 0x16 back to MCU (captured from fbxhome pcap)
				mux.SendRawCTRL([]byte{opStopPairing})
				logger.Info("pairing: stop echo sent (0x16)")
				if address > 0 {
					goto config
				}
				continue
			}

			// PKT heartbeat from sensor (arrives after CTRL completes)
			if address > 0 && len(evt.Data) >= 2 && evt.Data[1] == byte(address) {
				logger.Info("pairing: sensor heartbeat!", "hex", fmt.Sprintf("%x", evt.Data))
				SendConfig(ctx, mux, uint32(address), model, events, logger)
				// Watchdog to persist pairing in NVM (same as config path)
				time.Sleep(2 * time.Second)
				mux.SendWatchdog()
				logger.Info("pairing: watchdog commit sent")
				goto done
			}

			if op != opBeacon {
				logger.Info("pairing: event", "opcode", fmt.Sprintf("0x%02x", evt.Data[0]), "len", len(evt.Data))
			}
		}
	}

config:
	logger.Info("pairing: sending config immediately (no heartbeat wait)")
	time.Sleep(500 * time.Millisecond)
	SendConfig(ctx, mux, uint32(address), model, events, logger)

	// Send watchdog 0x05 to commit pairing to NVM.
	// Captured from fbxhome: sent on 8005→8004 ~13s after config.
	// Without this, the pairing doesn't persist after MCU reboot.
	time.Sleep(2 * time.Second)
	mux.SendWatchdog()
	logger.Info("pairing: watchdog commit sent")

done:
	vendorName := ""
	if matchedKey != nil {
		vendorName = matchedKey.Name
	}
	logger.Info("pairing: complete", "model", model, "vendor", vendorName, "address", address)

	return &PairingResult{
		DeviceUID:  uid,
		Model:      model,
		VendorName: vendorName,
		Address:    address,
	}, nil
}

// sendConfig sends bytecode + config managed frames to the sensor.
// Called immediately after detecting the sensor's heartbeat.
// KPDConfigPayload returns the config payload for a KPD sensor.
// If kpdCode is a 4-digit PIN, mode-4 (PIN required) is configured.
// Otherwise mode-2 (no PIN) is used.
func KPDConfigPayload(kpdCode string) []byte {
	if len(kpdCode) == 4 {
		var hash byte
		for _, c := range kpdCode {
			if c >= '0' && c <= '9' {
				hash += byte(c - '0')
			}
		}
		hash += 4
		return []byte{0x55, 0x00, 0x04, hash, 0x00, 0x00, 0x00}
	}
	return []byte{0x55, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00}
}

// qiaraTimestamp returns the current time as a 4-byte LE uint32,
// seconds since 2018-01-01 00:00:00 UTC (Qiara epoch).
func QiaraTimestamp() []byte {
	const qiaraEpoch = 1514764800 // 2018-01-01 00:00:00 UTC
	ts := uint32(time.Now().Unix() - qiaraEpoch)
	return []byte{byte(ts), byte(ts >> 8), byte(ts >> 16), byte(ts >> 24)}
}

// kpdBCD encodes a 4-digit PIN as packed BCD per fbxhome convention.
// BCD table: 0->0x0A, 1->0x00, 2->0x01, ..., 9->0x08.
// Packed: even-index digit in low nibble, odd-index digit in high nibble.
// KpdBCDPublic encodes a numeric PIN as BCD bytes (exported for reuse).
func KpdBCDPublic(code string) []byte {
	return kpdBCD(code)
}

func kpdBCD(code string) []byte {
	bcdTable := [10]byte{0x0A, 0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	var out []byte
	for i, c := range code {
		d := int(c - '0')
		if d < 0 || d > 9 {
			continue
		}
		b := bcdTable[d]
		if i%2 == 0 {
			out = append(out, b) // low nibble
		} else {
			out[len(out)-1] |= b << 4 // high nibble
		}
	}
	return out
}

// SendConfig sends bytecode + config managed frames to a sensor.
// Used both after initial pairing and for re-init after boot.
// If reinit is true, the heartbeat-ack is skipped (already sent by the event handler)
// and the first bytecode frame is sent immediately without waiting for a sensor request.
func SendConfig(ctx context.Context, mux *charmux.Client, addr uint32, model string, events <-chan charmux.Event, logger *slog.Logger) {
	sendConfig(ctx, mux, addr, model, "", 0, 0, false, 0, false, events, logger, false)
}

// SendConfigWithCode is like SendConfig but with an optional KPD PIN code.
func SendConfigWithCode(ctx context.Context, mux *charmux.Client, addr uint32, model string, kpdCode string, events <-chan charmux.Event, logger *slog.Logger) {
	sendConfig(ctx, mux, addr, model, kpdCode, 0, 0, false, 0, false, events, logger, false)
}

// SendConfigReinit is like SendConfig but for re-init after boot.
// Skips heartbeat-ack and sends bytecode immediately.
func SendConfigReinit(ctx context.Context, mux *charmux.Client, addr uint32, model string, events <-chan charmux.Event, logger *slog.Logger) {
	sendConfig(ctx, mux, addr, model, "", 0, 0, false, 0, false, events, logger, true)
}

// SendConfigReinitWithCode is like SendConfigReinit but with an optional KPD PIN code.
// initialAckCnt is the sensor's counter from the f1 heartbeat (used for heartbeat-ack).
// startCounter is the gateway's frame counter (must be high enough for the MCU to accept).
// hasBytecode: if true, skip bytecode upload (sensor already has it from previous config).
// rfByte: radio config byte from the sensor's heartbeat (MCU needs this for TX channel).
func SendConfigReinitWithCode(ctx context.Context, mux *charmux.Client, addr uint32, model string, kpdCode string, initialAckCnt uint32, startCounter uint32, hasBytecode bool, rfByte byte, events <-chan charmux.Event, logger *slog.Logger) {
	sendConfig(ctx, mux, addr, model, kpdCode, initialAckCnt, startCounter, hasBytecode, rfByte, false, events, logger, true)
}

// SendConfigReinitFull is like SendConfigReinitWithCode with a fromFNV flag for debugging.
// fromFNV indicates the reinit was triggered by an FNV frame (not a f1 heartbeat).
func SendConfigReinitFull(ctx context.Context, mux *charmux.Client, addr uint32, model string, kpdCode string, initialAckCnt uint32, startCounter uint32, hasBytecode bool, rfByte byte, fromFNV bool, events <-chan charmux.Event, logger *slog.Logger) {
	sendConfig(ctx, mux, addr, model, kpdCode, initialAckCnt, startCounter, hasBytecode, rfByte, fromFNV, events, logger, true)
}


func sendConfig(ctx context.Context, mux *charmux.Client, addr uint32, model string, kpdCode string, initialAckCnt uint32, startCounter uint32, hasBytecode bool, rfByte byte, fromFNV bool, events <-chan charmux.Event, logger *slog.Logger, reinit bool) {
	a := byte(addr)
	cnt := startCounter

	type frame struct {
		name    string
		wflags  byte
		ackCnt  uint32
		payload []byte
		flags   uint16 // 0 = use default 0x0547
		timeout time.Duration // 0 = default 5s
	}
	// AckCnt must increment: 0, 1, 2, 3, 4, 5 (captured from fbxhome)
	// Send bytecode, then config. The end-marker has a dynamic payload
	// that we can't compute yet — try sending config directly after bytecode.
	var frames []frame
	if reinit {
		// During reinit, send a single heartbeat-ack (fbxhome only sends one).
		// Sending multiple ACKs confuses the KPD.
		raw := buildManagedRawFull(a, cnt, initialAckCnt, 0xCC, []byte{0x78}, 0x0547, 0)
		logger.Info("reinit: heartbeat-ack sent", "ackCnt", initialAckCnt, "rfByte", fmt.Sprintf("0x%02x", rfByte), "hex", fmt.Sprintf("%x", raw))
		if err := mux.SendPKT(ctx, raw); err != nil {
			logger.Warn("reinit: heartbeat-ack send failed", "error", err)
			return
		}
		cnt++
	} else {
		// During initial pairing, send heartbeat-ack in the dialog (wait for sensor request).
		frames = append(frames, frame{"heartbeat-ack", 0xCC, 0, []byte{0x78}, 0, 0})
	}

	chunks, ok := BytecodeChunks(model)
	if !ok {
		logger.Warn("pairing: no bytecode for sensor model")
		return
	}
	nChunks := len(chunks)
	if !hasBytecode {
		// Sensor needs bytecode — send all chunks.
		for i, chunk := range chunks {
			frames = append(frames, frame{
				name:    fmt.Sprintf("bytecode-%d", i+1),
				wflags:  0xCD,
				ackCnt:  uint32(i + 1),
				payload: chunk,
			})
		}
	} else {
		logger.Info("reinit: sensor has bytecode, skipping upload")
	}
	// End-marker (WFlags=0xC8): payload is the current timestamp as uint32 LE,
	// seconds since 2018-01-01 00:00:00 UTC (Qiara epoch). Discovered by
	// correlating fbxhome "send time" log with pcap end-marker bytes.
	prefix := ""
	if len(model) >= 10 {
		prefix = model[:10]
	}
	endMarkerPayload := QiaraTimestamp()
	frames = append(frames, frame{"end-marker", 0xC8, uint32(nChunks + 1), endMarkerPayload, 0, 0})
	// Config payload varies by sensor type.
	// DWS/PIR: 55 00 00. KPD: mode-2 (55 00 02) or mode-4 (55 00 04 hash) depending on kpdCode.
	// SRN: 55 06.
	// Note: 3rd byte is 0x00 (value captured from fbxhome). 0x01 caused
	// a f1ff reinit loop on the DWS after re-pairing.
	configPayload := []byte{0x55, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	switch prefix {
	case "HOMELABSRN":
		configPayload = []byte{0x55, 0x06}
	case "HOMELABKPD":
		configPayload = KPDConfigPayload(kpdCode)
	}
	frames = append(frames, frame{"config", 0x01, uint32(len(chunks) + 2), configPayload, 0, 0})
	// Post-config frames (captured from fbxhome).
	postIdx := uint32(len(chunks) + 3)
	switch prefix {
	case "HOMELABSRN":
		frames = append(frames, frame{"srn-status", 0x0a, postIdx, []byte{0x02, 0x55, 0x0f}, 0x0d43, 0})
	case "HOMELABKPD":
		kpdPostPayload := []byte{0x00, 0x00}
		if kpdCode != "" {
			bcd := kpdBCD(kpdCode)
			kpdPostPayload = make([]byte, 0, 3+len(bcd))
			kpdPostPayload = append(kpdPostPayload, 0x03, 0x00, byte(len(kpdCode)))
			kpdPostPayload = append(kpdPostPayload, bcd...)
		}
		frames = append(frames, frame{"kpd-post", 0x01, postIdx, kpdPostPayload, 0, 60 * time.Second})
	}

	// Send all frames rapidly without waiting.
	// The sensor wakes up after the first frame and catches subsequent ones.
	// fbxhome sends all 6 frames in ~700ms total (~130ms spacing).
	// Dialog: wait for sensor request, respond with next frame.
	// fbxhome flow: sensor heartbeat → ack → sensor FNV request → bytecode1 → ...
	// Each frame is sent IN RESPONSE to a sensor packet.
	var lastSensorCnt uint32
	for _, f := range frames {
		var sensorCnt uint32

		// Special case: kpd-post and srn-status are emitted IMMEDIATELY after
		// config (using the last sensor counter), without waiting for a real
		// sensor request. The KPD/SRN expect them as one-shot — not a dialog.
		// Without this shortcut, the frame times out and the sensor is left
		// half-configured (silent for KPD, no audio for SRN).
		if (f.name == "kpd-post" || f.name == "srn-status") && lastSensorCnt > 0 {
			sensorCnt = lastSensorCnt + 2 // next expected counter from sensor side
			logger.Info("pairing: sending post-config frame immediately", "frame", f.name, "reusedCnt", sensorCnt)
		} else {
			// Wait for sensor packet — only accept frames FROM this sensor (GWSrc == addr).
			timeout := 5 * time.Second
			if f.timeout > 0 {
				timeout = f.timeout
			}
			timer := time.NewTimer(timeout)
		waitLoop:
			for {
				select {
				case evt := <-events:
					parsed, err := charmux.DeserializeManagedFrame(evt.Data)
					if err != nil || parsed.GWSrc != addr {
						if len(evt.Data) >= 2 {
							logger.Debug("pairing: skipping non-target event", "frame", f.name, "hex", fmt.Sprintf("%x", evt.Data))
						}
						continue
					}
					sensorCnt = parsed.Counter
					logger.Info("pairing: sensor request", "frame", f.name, "len", len(evt.Data), "hex", fmt.Sprintf("%x", evt.Data))
					break waitLoop
				case <-timer.C:
					logger.Warn("pairing: no sensor request before", "frame", f.name)
					break waitLoop
				}
			}
			timer.Stop()

			// A sensor boot cycle (Counter=0) is legitimate for the very first
			// frame after a cold start. Only skip if we already saw a higher
			// counter previously — otherwise we'd drop the heartbeat-ack for
			// every freshly-paired sensor and leave it in half-configured state.
			if sensorCnt == 0 && lastSensorCnt > 0 {
				logger.Warn("pairing: skipping frame (no valid ackCnt)", "frame", f.name)
				cnt++
				continue
			}
			lastSensorCnt = sensorCnt
		}

		// Respond with our frame — ackCnt must match the sensor's actual counter.
		flags := uint16(0x0547)
		if f.flags != 0 {
			flags = f.flags
		}
		raw := buildManagedRawFull(a, cnt, sensorCnt, f.wflags, f.payload, flags, rfByte)
		if err := mux.SendPKT(ctx, raw); err != nil {
			logger.Warn("pairing: PKT send failed", "frame", f.name, "error", err)
			return
		}
		logger.Info("pairing: PKT sent", "frame", f.name, "len", len(raw), "hex", fmt.Sprintf("%x", raw), "ackCnt", sensorCnt)
		cnt++
	}
}

// buildManagedFrame builds a ManagedFrame struct.
func buildManagedFrame(dst byte, counter uint32, ackCnt uint32, wflags byte, payload []byte, flags uint16) *charmux.ManagedFrame {
	return &charmux.ManagedFrame{
		GWDst:   uint32(dst),
		GWSrc:   1,
		RFByte:  0,
		Counter: counter,
		Src:     1,
		Flags:   flags,
		AckDst:  uint32(dst),
		AckCnt:  ackCnt,
		WFlags:  wflags,
		Payload: payload,
	}
}

// buildManagedRaw builds a raw serialized managed frame with default fbxhome flags (0x0547).
func buildManagedRaw(dst byte, counter uint32, ackCnt uint32, wflags byte, payload []byte) []byte {
	return buildManagedRawFull(dst, counter, ackCnt, wflags, payload, 0x0547, 0)
}

// buildManagedRawFlags builds a raw serialized managed frame with specified flags.
func buildManagedRawFlags(dst byte, counter uint32, ackCnt uint32, wflags byte, payload []byte, flags uint16) []byte {
	return buildManagedRawFull(dst, counter, ackCnt, wflags, payload, flags, 0)
}

// buildManagedRawFull builds a raw serialized managed frame with all parameters.
func buildManagedRawFull(dst byte, counter uint32, ackCnt uint32, wflags byte, payload []byte, flags uint16, rfByte byte) []byte {
	f := &charmux.ManagedFrame{
		GWDst:   uint32(dst),
		GWSrc:   1,
		RFByte:  rfByte,
		Counter: counter,
		Src:     1,
		Flags:   flags,
		AckDst:  uint32(dst),
		AckCnt:  ackCnt,
		WFlags:  wflags,
		Payload: payload,
	}
	return f.Serialize()
}

// SendSRNCommand sends an actuator command to a SRN (siren) sensor.
// Format reverse-engineered from fbxhome HlSrn::virtual_64 @ 0xab7b8:
//   payload = 55 05 01 <test_power> <(test_duration<<2)&0xfc>   (5 bytes)
//   defaults: test_power=10, test_duration=10 → final byte 0x28
//   flags=0x0883 (no ACK), wflags=addr byte.
//
// IMPORTANT: alarm_ring (epByte=0x07) is NOT a direct write — fbxhome
// triggers it through HlSrn::set_state_to(ARMED) state machine. Sending
// this format with epByte=0x07 will likely be ignored by the SRN bytecode.
// Pending RE of set_state_to @ 0xab51c.
func SendSRNCommand(ctx context.Context, mux *charmux.Client, addr uint32, command string, counter uint32) error {
	var epByte byte
	switch command {
	case "test":
		epByte = 0x05
	case "alarm_ring":
		epByte = 0x07
	default:
		return fmt.Errorf("unknown SRN command: %s", command)
	}

	raw := buildManagedRawFlags(byte(addr), counter, 0, byte(addr), []byte{0x55, epByte, 0x01, 0x0a, 0x28}, 0x0883)
	return mux.SendPKT(ctx, raw)
}

// SendKPDPassword sends a 55 04 NVM write frame to program the PIN into the KPD.
// Captured format: flags=0x0d43, wflags=addr, payload = 01 55 04 [hash] [hash]
// [xor-encoded digits] [padding] 03 [padding] 03.
// XOR key derived from pcap analysis of "5678": A3 33 53 3A.
func SendKPDPassword(ctx context.Context, mux *charmux.Client, addr uint32, kpdCode string, counter uint32) error {
	if len(kpdCode) != 4 {
		return fmt.Errorf("KPD code must be 4 digits, got %q", kpdCode)
	}
	var hash byte
	for _, c := range kpdCode {
		if c >= '0' && c <= '9' {
			hash += byte(c - '0')
		}
	}
	hash += 4

	// XOR key for digit encoding (from pcap RE of kpd_pwd_5678b.pcap).
	xorKey := [4]byte{0xa3, 0x33, 0x53, 0x3a}
	var encoded [4]byte
	for i := 0; i < 4; i++ {
		encoded[i] = kpdCode[i] ^ xorKey[i]
	}

	// Full payload: 01 55 04 [hash×2] [4 encoded digits] [7 zeros] 03 [6 zeros] 03
	payload := make([]byte, 24)
	payload[0] = 0x01
	payload[1] = 0x55
	payload[2] = 0x04
	payload[3] = hash
	payload[4] = hash
	copy(payload[5:9], encoded[:])
	// bytes 9-15 = 0x00 (already zeroed)
	payload[16] = 0x03
	// bytes 17-22 = 0x00 (already zeroed)
	payload[23] = 0x03

	raw := buildManagedRawFlags(byte(addr), counter, 0, byte(addr), payload, 0x0d43)
	return mux.SendPKT(ctx, raw)
}

func trimNull(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}
