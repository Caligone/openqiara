# KPD (Keypad) Protocol

This document describes the complete DomusRF protocol for the Qiara KPD (HOMELABKPD),
as reverse-engineered from fbxhome and validated in openqiara.

## Overview

The KPD is a battery-powered keypad with 3 buttons (ON, Moon/Night, Star) plus a 10-key numeric pad.
It communicates over DomusRF (868 MHz sub-GHz radio) through the camera MCU via the charmux UDP channel.

Key characteristics:
- **Address**: typically `20` (0x14) in hex
- **Model**: `HOMELABKPD00ACFD`
- **Radio state**: the KPD is quasi-always in "low-power listening" mode and only actively transmits:
  - On user action (button/code)
  - On periodic `5509` heartbeats that precede user actions (wake pattern)
  - On `f1...` reinit heartbeats after boot (power cycle)
- **No deep sleep recovery**: once the KPD enters FNV verification mode, only a power cycle gets it out

## PKT frame format recap

All radio traffic uses **PKT managed frames** via charmux UDP ports 8002/8003:

```
[GWDst:varint][GWSrc:varint][RFByte:u8][Counter:varint (LSL 1)][Src:varint]
[Flags:u16 LE][AckDst:varint?][AckCnt:varint?][WFlags:u8?][Payload:bytes?]
```

- `AckDst`/`AckCnt` present iff `FlagA` (0x0004) is set in Flags
- `WFlags` + `Payload` present iff `FlagW` (0x0002) is set in Flags

Flag values commonly seen (as `u16 LE`):
- `0x0547` = ZWAE, managed frame with payload + ack (`fbxhome "Sent manage:"`)
- `0x0544` = AE, pure ACK no payload (`fbxhome "Sent:" pureAck`)
- `0x0D43` = ZWE actuator command, no ack expected (`fbxhome "Sent:" with rt:XX`)
- `0x0083` = ZW, sensor→gateway with payload but no ack field
- `0x0085` = ZA (variation seen in responses)

## Initialization flow (reinit after battery cycle)

After a battery cycle, the KPD sends periodic `f1ff01` or `f10001` heartbeats until
the gateway responds and starts the reinit dialog. The sequence below is observed in
both fbxhome and openqiara and MUST be followed exactly:

```
KPD → GW  : f1ff01 (or f10001)                                         ; reinit heartbeat
GW  → KPD : heartbeat-ack, flags=0x0547, wflags=0xCC, payload=0x78     ; a single one!
KPD → GW  : d1789c80fd8e4fa28e5d 000000000000000000 (FNV hash = zeros) ; bytecode check
GW  → KPD : bytecode-1 (0x80 chunk header + first 1024-bit chunk)
KPD → GW  : 0f00000000
GW  → KPD : bytecode-2
KPD → GW  : 0300000000
GW  → KPD : bytecode-3
KPD → GW  : 0300000000
GW  → KPD : bytecode-4
KPD → GW  : 0300000000
GW  → KPD : end-marker, wflags=0xC8, payload=<Qiara timestamp (4 bytes LE)>
KPD → GW  : flags=0x0085 ACK (empty payload)
GW  → KPD : config 0x0547, wflags=1, payload=55 00 04 <hash> 00 00 00  ; code hash
KPD → GW  : 5509                                                        ; first heartbeat
GW  → KPD : kpd-post 0x0547, wflags=1, payload=03 00 04 <BCD(code)>     ; the actual PIN
```

### Critical constraints

1. **Single heartbeat-ack**: fbxhome sends ONE heartbeat-ack per reinit. Sending 3 in a row
   (as we did initially) causes the KPD to reject the reinit entirely. It will keep sending
   `f1ff01` forever.

2. **Skip non-response frames**: during `sendConfig`, the KPD occasionally sends `f1ff01` or
   `e1ff01` heartbeats between frames. These are NOT responses — ignore them and wait for
   the actual data frame (FNV hash, `0f...`, `03...`, or `5509`). Otherwise the counter
   gets confused.

3. **kpd-post requires 5509**: the last frame (`kpd-post`) must be sent in response to a
   real `5509` heartbeat, not any other sensor-originated frame. This is why we filter
   explicitly on `payload[0:2] == [0x55, 0x09]` for that frame.

4. **Counter starts at 2**: fbxhome starts its gateway counter at 2 for the reinit
   (independent of the sensor's own counter). Low counters are NOT rejected by the sensor.

5. **No vendor key write**: fbxhome does NOT send a `0x01 + 32 bytes` vendor key frame on
   CTRL at boot. Sending it puts the MCU in a state where the KPD enters long radio sleep.

6. **No CTRL init beyond GetInfo/GetNet**: fbxhome only sends `0x02` (GetInfo) and `0x05`
   (GetNet) on the CTRL channel. That's it.

## Steady-state operation

After a successful reinit, the KPD stays quiet until user interaction. When the user
presses a button, the KPD sends a wake heartbeat followed by the actual event:

```
KPD → GW  : 5509                                                       ; wake heartbeat
GW  → KPD : kpd-post flags=0x0547, wflags=1, payload=03 00 04 <BCD>   ; critical: must respond
KPD → GW  : 5501 <timestamp:4> 0401                                    ; ON button pressed → armed_away
GW  → KPD : pure ACK flags=0x0544 (no wflags, no payload)              ; ackdst/ackcnt only
KPD → GW  : 5501 <timestamp:4> 84 00 <BCD:4> 0000                      ; code entered → disarmed
GW  → KPD : pure ACK flags=0x0544
```

Additional cycles reuse the same pattern. Between two cycles (from disarm to the next
ON press), the KPD can stay silent for arbitrarily long periods — fbxhome logs show
gaps of 18-28 minutes with no communication at all.

### 5501 event byte layout

```
55 01 <timestamp:4> <action_byte> [extra]
```

The timestamp is in the Qiara epoch (seconds since 2018-01-01 00:00:00 UTC) encoded
as 4 bytes LE. The `action_byte` at offset 6 of the payload indicates:

- `0x04` = simple button press, followed by `0x01` (ON/arm_away), `0x02` (Moon/arm_night),
  or `0x00` (disarm via button, not via code)
- `0x84` = PIN code entry (bit 7 set), followed by `0x00 <BCD:4> 00 00` → disarmed

For button presses, the button identity is the final byte. For code entries, we trust
the KPD — if the code is wrong the KPD does not emit a 5501 event at all.

## The FNV loop (what to AVOID)

The FNV verification mode is the failure state we spent most of this reverse-engineering
session chasing. When the KPD enters FNV loop, it sends 33-byte frames with:
- `Flags = 0x0547` with A bit set
- `WFlags = 0x81`
- Payload = `d178...<firmware hash> <bytecode hash> ff`

Once in this mode, the KPD no longer responds to normal commands (button LEDs may still
work locally but no radio frames are emitted). The only recovery is a battery cycle.

### Root cause identified

The KPD enters FNV loop when the gateway **fails to respond correctly** to a `5509` wake
heartbeat. Specifically:

- **Wrong**: sending a standard ACK (`flags=0x0547, wflags=0xCC, payload=0x78`) to a `5509`.
  This is what `ackPKTEvent` does for DWS/PIR. For the KPD, this is the wrong response.
- **Right**: responding to a `5509` with a `kpd-post` frame (`flags=0x0547, wflags=1,
  payload=03 00 04 <BCD>`). No standard ACK on top — the kpd-post IS the response.

This is implemented in `handlePKTEvent`:

```go
switch data[payloadStart+1] {
case 0x01:
    // 5501 = KPD button/code → pure ACK (no payload)
    pureAck = true
case 0x09:
    // 5509 = KPD heartbeat → skip ACK entirely, sendKPDPostResponse will handle it
    skipAck = true
}
if !skipAck {
    c.ackPKTEvent(data, pureAck)
}
```

And in the `isKPDHeartbeat` switch case, `sendKPDPostResponse` sends the kpd-post frame.

### Why FNV end-marker responses don't work

One of the ideas we tried was to respond to FNV frames with an end-marker
(`wflags=0xC8 + timestamp`) to tell the sensor "your bytecode is fine". This causes an
infinite loop — the KPD keeps sending FNV frames and expects the full bytecode or
nothing at all. **Never respond to FNV frames.** The only cure is to prevent the KPD
from entering that mode in the first place, by responding correctly to `5509` heartbeats.

## Gateway counter semantics

The gateway counter (`Counter` field) is a **single global 32-bit counter** shared across
ALL sensors, NOT per-sensor. This matters because:

1. fbxhome uses a global counter. Looking at captured traffic:
   - `cnt:10` → KPD pureAck
   - `cnt:11` → SRN actuator command
   - `cnt:12` → KPD pureAck
   - `cnt:13` → SRN actuator command
   - ...

2. The sensor sees "gaps" in its incoming counter sequence (10, 12, 14, ...) and this
   is expected — it knows other sensors are on the same bus.

3. Using a per-sensor counter (2, 3, 4 strictly sequential for just one sensor) seemed
   to cause the KPD to enter FNV faster in our tests, though the primary fix was the
   5509/kpd-post handling.

Implementation: `nextManagedCnt()` returns `c.manageCnt` then increments it. The counter
is initialized to `2` at startup and reset to `startCnt+10` after each reinit.

## Code management

The PIN code is stored in `config.SensorEntry.KPDCode` (4-digit string) and sent to the
KPD in two places during reinit:
- **Config frame** (`payload: 55 00 04 <hash> 00 00 00`): the hash is `sum_of_digits + 4`
  (e.g. `1+2+3+4 + 4 = 14 = 0x0E`)
- **kpd-post frame** (`payload: 03 00 04 <BCD:4>`): the code itself in BCD, two digits
  per byte with nibbles swapped (e.g. `1234` → `10 32`, `1903` → `80 2A`)

The same `kpd-post` payload is also sent on each `5509` heartbeat during steady-state
operation — this is what keeps the KPD out of FNV loop.

## What we still don't fully understand

1. **Why `5509` heartbeat frequency varies wildly**: fbxhome logs show heartbeats every
   ~45s, our logs sometimes show gaps of 1-3 minutes between heartbeats. This might be
   related to radio signal quality (`rf_sig` field in fbxhome logs) but we didn't
   correlate.

2. **The `8d000002d2000000` frame**: occasionally the KPD sends a 17-byte frame with
   `WFlags=0x8d`. Its meaning is unknown — possibly a tamper event or some status
   report. We currently ignore it.

3. **The SRN integration**: fbxhome automatically triggers the siren (`55 04 ...`
   pre-arm beep, `55 05 00 84` stop) on each KPD arm/disarm. OpenQiara now drives
   this from the alarm engine via `SendSirenAlarm` — see [`sensors.md`](sensors.md).

## Testing checklist

When making changes to the KPD handling, validate the following scenarios:

1. **Fresh reinit after battery cycle**: white LED → reinit completes
2. **Immediate arm/disarm**: ON + code works right after reinit
3. **Quick succession**: arm → disarm → arm → disarm works without waiting
4. **30s idle**: arm → wait 30s → disarm works
5. **2min idle**: arm → wait 2min → arm → disarm works
6. **5min+ idle**: same but with longer gaps — this is where the FNV loop used to trigger
7. **Log check**: after a full test cycle, verify NO `wflags:0x81` (FNV) frames were
   received. If any appear, the KPD has entered failure mode.
