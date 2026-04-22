# DomusRF Protocol — Reference

This document describes the proprietary **DomusRF 868 MHz** radio protocol used
by Qiara sensors, the **charmux** UART transport between the camera's Linux
SoC and its 868 MHz MCU, and the **managed frame** format used to talk to
sensors.

> Companion document: [`docs/re-findings.md`](re-findings.md) — the research
> log behind this reference, including verified findings, open questions, and
> the parts of `fbxhome` that are still incompletely understood.

> ## Legal note
>
> This documentation is the result of black-box observation of network traffic
> and runtime behaviour, performed for **interoperability** purposes under EU
> Directive 2009/24/EC (Article 6) and French CPI L122-6-1.IV.
>
> No proprietary code, firmware, or cryptographic material from Qiara, Free,
> Iliad, Cofidur EMS, or Sigmastar is reproduced or distributed in this
> document or in OpenQiara. The information here is sufficient to interoperate
> with sensors you already own; it is not sufficient to clone or counterfeit
> Qiara products.

---

## 1. Hardware overview

| Component | Role |
|-----------|------|
| Sigmastar SSC SoC | Linux 4.9, ARM Cortex-A7, runs `openqiarad` |
| EZR32LG MCU | ARM Cortex-M3 + Si446x radio, runs MutekH RTOS, handles 868 MHz radio |
| UART `/dev/ttyS2` | Single serial line between SoC and MCU, multiplexed by `charmux` |

The MCU is **always** the radio side. The SoC never speaks 868 MHz directly —
all sensor I/O goes through the MCU.

---

## 2. Charmux — UART multiplexer

`charmux` is a vendor binary (`/usr/bin/charmux`) that takes the single UART
to the MCU and exposes each logical channel as a pair of UDP sockets on
localhost. OpenQiara depends on this binary; replacing it would require
re-implementing the UART framing in Go (~300 lines, see issue tracker).

### 2.1 Channels

| Channel | SoC port | MCU port | Purpose |
|---------|----------|----------|---------|
| 0 (CTRL) | 8001 | 8000 | Control commands and replies |
| 1 (PKT)  | 8003 | 8002 | Sensor data packets (encrypted) |
| 2 (Shutter) | 8007 | 8006 | Camera privacy shutter |
| 3 (Watchdog) | 8005 | 8004 | MCU keepalive |

To send a CTRL command from Go: `UDP send to 127.0.0.1:8000`. The MCU's
reply arrives at `127.0.0.1:8001`.

### 2.2 Boot sequence

After charmux is up and the MCU has been flashed by `uartboot`, the SoC must:

1. Send `0x05` on Watchdog (port 8004) — enables managed frame forwarding
2. Send `0x02` on Shutter (port 8006) — initialise shutter state
3. CTRL: send `GetInfo` (`0x02`) and `GetNet` (`0x05`) — negotiate state with MCU
4. PKT: send an initial ACK (FlagA) — enables PKT event forwarding upstream

Until step 4 is done, **no sensor events reach the SoC** even if sensors are
already paired and active.

### 2.3 Shutter

Direct byte commands on port 8006:

| Byte | Action |
|------|--------|
| `0x01` | Open shutter |
| `0x02` | Close shutter |

(Yes, the CTRL channel inverts these — historical artifact of the fbxhome
HTTP API. OpenQiara always uses the direct charmux byte.)

---

## 3. CTRL protocol

Each CTRL message has a **routing bit** (bit 0 of the message struct at
offset `0x50` inside the MCU memory):

- **bit 0 = 0** → Local mode: MCU dispatches the opcode internally
- **bit 0 = 1** → Relay mode: MCU forwards the message to/from the radio

OpenQiara uses local mode for everything except the pairing handshake.

### 3.1 Local-mode opcodes

| Opcode | Direction | Function | Reply |
|--------|-----------|----------|-------|
| `0x02` | → MCU | `GET_INFO` | 8 bytes: `[op, netid_lo, netid_hi, addr, flags×3, state]` |
| `0x05` | → MCU | `GET_NET` | 1 byte ACK |
| `0x07` | → MCU | `GET_NODES` | 74 bytes (paired-node table) |
| `0x13` | → MCU | `START_PAIRING` (internal) | `0x14` on success |
| `0x01` | → MCU | Config write zone 1 (32 B) | `0x04` |

> ### ⚠️ DANGEROUS opcodes
>
> - **`0x03`** (config write zone 3) **crashes the MCU and corrupts NVM**
>   on every call. The state field of `GET_INFO` flips from `0x1d` (ready)
>   to `0x0e`, then `0x08` (broken), and the MCU has to be reflashed by
>   `uartboot` at next boot.
> - **`0x08`** crashes the MCU as well.
> - Do not extend the firmware page count past **89 pages** — pages above
>   that overwrite the bootloader, not the NVM, and brick the MCU.
>
> OpenQiara never issues these opcodes by design — do not extend the code paths that call `SendRawCTRL` without a matching safety review.

### 3.2 GET_INFO state field

The last byte of `GET_INFO` is the MCU init state:

| Value | Meaning |
|-------|---------|
| `0x08` | Boot / partial init — only opcodes 0x02-0x07 work, sensors silent |
| `0x09` | Intermediate — pairing CTRL works, PKT heartbeat forwarding works, `SET_KEY` (0x04) returns error 0x86. Production cameras with fbxhome disabled stabilise here |
| `0x0e` | Post-corruption — observed after a stray `0x03` to an already-provisioned MCU |
| `0x1d` | Fully initialised — everything works |

The transition `0x08 → 0x1d` happens **only** when the MCU is at `addr=0`
(virgin). fbxhome detects this in dispatcher case `0x02` and emits opcode
`0x03` with payload `[netid_lo, netid_hi, varint(1), 0x00]` (5 bytes
post-opcode), where `netid` is a freshly generated random 16-bit value
with popcount ≥ 4. Once the MCU has stored its `addr`, this branch is
never taken again; OpenQiara cannot drive the transition itself for an
already-provisioned camera. State `0x09` is good enough for routine use.

---

## 4. Pairing

There are two paths to pair a sensor: the legacy **fbxhome HTTP API** (used
by `internal/camera/fbxhome.go` when running in proxy mode) and the **direct
charmux handshake** (used in `mode=charmux` standalone).

### 4.1 fbxhome HTTP API (legacy mode)

```http
POST /api/v1/home/pairing
Header: X-Hlcore-Session-Id: <session>

Start: {"op": "start_adapter", "node_type": "HOMELABDWS",
        "adapter_type": "Adapter.DomusAdapter"}
Poll:  {"op": "poll", "session": <id>}
Stop:  {"op": "stop", "session": <id>}
```

Authentication: get a session via `fbxbusctl call create_login_session 1 1`,
then pass the returned token in `X-Hlcore-Session-Id`.

### 4.2 Direct charmux handshake (standalone mode)

This is the path OpenQiara uses by default. It bypasses fbxhome entirely.

**Step A — CTRL handshake:**

1. Compute `next_addr = max(known sensor addresses) + 1`. Skip address 1 (gateway) and any deleted address.
2. Send `0x15` on CTRL: `15 [next_addr] 00 00 00 [next_addr] 00 00 00 00 00 00 00 00 00 00 00 00` (18 bytes)
3. Wait for beacon `0x17` from the MCU (47 bytes): `17 [vendor_prefix 6B] [sensor_uid 8B] [model 16B ASCII] [padding]`
4. Match the vendor prefix against `/etc/hl/vendors.keys` (Qiara sensors use `cofidur1` for DWS, `cofidur3` for KPD; key file format is `name: base64(32 bytes)`)
5. Send `0x1a` (57 bytes): `1a [sensor_uid 8B] [vendor_key 32B] [16B zeros]`
6. Wait for `0x1f` (49 bytes): `1f [sensor_uid 8B] [net_data 24B] [node_key 16B]` — challenge from MCU
7. Send `0x1c` (9 bytes): `1c [sensor_uid 8B]` — confirmation
8. Wait for `0x1e` (13 bytes): `1e [sensor_uid 8B] [node_id 4B LE]` — pairing complete with assigned node ID
9. Send `0x16` (1 byte) — STOP_PAIRING

**Step B — PKT bytecode push (sensor configuration):**

After the CTRL handshake the MCU knows the sensor exists, but the sensor is
not yet operational — it's waiting for its bytecode (a small VM program that
declares its endpoints, signals, and behaviour). The bytecode push is a
strict request-response dialog over PKT, driven by the sensor:

1. Sensor sends its first heartbeat with `wflags=0xf1, payload=0xff` ("need everything")
2. Gateway replies `wflags=0xCC, payload=0x78` (heartbeat-ack with config offer)
3. Sensor sends an FNV-hashed request (29 bytes) → reply `wflags=0xCD` + bytecode chunk 1
4. Sensor sends a 15-byte request → reply chunk 2
5. Sensor sends a 15-byte request → reply chunk 3
6. Sensor sends a 15-byte request → reply `wflags=0xC8` (end marker)
7. Sensor sends a 9-byte ACK → reply `wflags=0x01, payload=55 00 01 00 00 00 00` (final config write)

**Critical timing rules:**

- Each frame **must** be sent in response to a sensor request, not on a timer. ~130 ms between exchanges is normal.
- The PKT counter on the gateway side must be incremented monotonically across all frames in the dialog.
- After the dialog completes, send watchdog `0x05` on port 8004 to keep PKT forwarding alive.
- The dialog is single-shot: if it fails, the sensor must be re-paired (CTRL handshake again).

The bytecode itself is sensor-type-specific. See `internal/domus/bytecode.go`
for the OpenQiara-shipped bytecode tables (DWS, PIR, KPD, SRN).

### 4.3 Pairing persistence

- **Pairing 0x15/0x1a (charmux)** persists in MCU NVM. After a reboot, the
  sensor is still associated and will resume sending events without
  re-pairing.
- **Bytecode (PKT)** does **not** persist on the gateway side — it must be
  re-pushed after every reboot. OpenQiara replays the bytecode for every
  known sensor at startup.
- **Sensors saved in `openqiara.json`** are reloaded at boot, but the actual
  cryptographic association lives in the MCU NVM. Deleting a sensor from
  `openqiara.json` does **not** remove it from the MCU; you must also send
  a deletion frame (TODO).

---

## 5. Managed frames (PKT channel)

The PKT channel transports **managed frames** — the structured packet format
used for sensor events, configuration, and bytecode upload. Raw bytes on PKT
are silently dropped by the MCU; you must use the managed frame format.

### 5.1 Wire encapsulation

Each managed frame is split into **8-byte chunks** on the wire, each prefixed
with `0x1C`, and terminated by `0x1D`:

```
0x1C [8 bytes]    # chunk 1
0x1C [8 bytes]    # chunk 2
...
0x1D              # end of frame
```

### 5.2 Frame body — varint TLV

The frame body uses **protobuf-style varints** (7 bits per byte, MSB =
continuation):

| Field | Encoding | Notes |
|-------|----------|-------|
| `gwdst`  | varint | Destination gateway address |
| `gwsrc`  | varint | Source gateway address (1 = us) |
| `rfbyte` | raw u8 | Always 0 in TX |
| `cnt*2`  | varint | Counter shifted left by 1 |
| `src`    | varint | Source ID |
| `flags`  | u16 LE | Two raw bytes, **not** varint |
| `ackdst` | varint | ACK destination |
| `ackcnt` | varint | ACK counter |
| `wflags` | raw u8 | Only present if flags bit 1 (W) is set |
| `payload`| raw bytes | Only present if flags bit 1 (W) is set |

### 5.3 Flags bitfield

| Bit | Letter | Meaning |
|-----|--------|---------|
| 0 | Z | Type bit 0 |
| 1 | W | WAKE — frame carries a payload |
| 2 | A | ACK |
| 3 | U | (unknown) |
| 4 | P | (unknown) |
| 5 | T | (unknown) |
| 6-9 | — | Manage class/subtype (4 bits) |
| 10 | E | (unknown) |
| 11-13 | — | Route count (3 bits) |
| 14-15 | — | Reserved |

Common combinations:

| Hex | Meaning |
|-----|---------|
| `0x0547` | ZWAE — managed frame with payload + ACK + extension |
| `0x0544` | Pure ACK frame |
| `0x0D43` | Actuator command (siren, shutter, etc.) |

### 5.4 Notable wflags values

| wflags | Direction | Meaning |
|--------|-----------|---------|
| `0xf1` | sensor → gw | Sensor needs everything (first heartbeat after pair) |
| `0xCC` | gw → sensor | Heartbeat ACK with config offer |
| `0xCD` | gw → sensor | Bytecode chunk |
| `0xC8` | gw → sensor | Bytecode end marker |
| `0x01` | gw → sensor | Final config write |
| `0x82` | sensor → gw | Battery report (rare; observed once on PIR low-battery) |
| `0x02` | sensor → gw | Heartbeat (alive ping) |

---

## 6. Sensor events (PKT inbound)

When a sensor reports something (motion, door open, button press), the MCU
receives the radio frame, decrypts it, and forwards a managed frame upstream
to the SoC on port `8003`.

### 6.1 Event header

After charmux strips the chunk wrapping, the inbound payload has this shape:

```
01 ADDR F0 xx ADDR ADDR FLAGS 00 01 55 [signal payload]
```

| Offset | Field | Notes |
|--------|-------|-------|
| 0 | direction byte (`0x01` = inbound) |
| 1 | sensor radio address |
| 2 | `0xF0` marker |
| 3 | (varies) |
| 4-5 | sensor address (echo) |
| 6 | flags |
| 7-8 | `00 01` |
| 9 | `0x55` (signal-payload marker) |
| 10+ | signal TLV (see 6.2) |

### 6.2 Signal TLV (the `55 ...` payload)

After the `0x55` marker, the rest of the payload is a **TLV-like signal stream**:

```
[header byte] [opcode byte] [data...]
```

The opcode byte is the **signal ID** declared in the sensor's bytecode. For
DWS sensors, signal IDs are in the range `0..10`. The data layout depends on
the opcode.

| Opcode | Name | Data |
|--------|------|------|
| `0x01` | STATE | 4 bytes timestamp/seq + 1 byte split (top 8 bits + bottom 6 bits) + 1 byte state value (0/1/2) |
| `0x07` | (unknown 1-byte field) | 1 byte |
| `0x08` | (unknown 1-byte field) | 1 byte |
| `0x09` | END_MARKER | (no data) |
| `0x0A` | TEMPERATURE | 1 byte raw |
| `0x02..0x06` | invalid (parser throws) | — |

**Notes:**

- **`STATE` (opcode 1)** is the main event opcode for DWS (open/closed/tamper)
  and PIR (motion/idle/tamper). The state byte takes values 0, 1, or 2.
- **`TEMPERATURE` (opcode 10)** is a raw byte. The conversion to °C is **not
  yet known** and is performed by fbxhome at HTTP-serialisation time, not at
  parse time. OpenQiara currently exposes the raw byte; a contribution to
  reverse the conversion is welcome.
- **Battery level** is **not** transmitted in normal operation. fbxhome
  obtains battery levels via the Sigfox cloud API (`HlSrn::send_get_sf_info`)
  which is offline since Qiara shut down. A `wflags=0x82` frame has been
  observed once for a low-battery PIR warning, but no continuous reporting
  exists in charmux mode.

### 6.3 KPD events

The keypad uses opcode `0x55 09` heartbeats and `0x55 04` button events. See
[`docs/kpd.md`](kpd.md) for the full KPD protocol details — it has its own
quirks (FNV verification loop, single-PIN limitation, ~60 s wake window).

### 6.4 Status heartbeats (`f1 XX YY`) — WARNING: not opcodes

A short PKT frame (11-14 bytes) that ends in `… f1 XX YY` is **not** three
fixed sub-opcodes. The bytes after `f1` are a continuation of a single
**LEB128 varint** carrying the sensor's `status_flags`. Decoding examples:

```
… 82 f1 00 01  → varint 0x71  (= bits 0,4,5,6) — "almost in sync"
… 82 f1 db 01  → varint 0x6df1               — "many things still pending"
… 82 f1 ff 01  → varint 0x7ff1               — "need everything" (post-battery flood)
```

The relevant bits, decoded by `parse_status` and the surrounding handler in
`fbxhome` (RE addr `0x89e78` / `0x8b3c4`):

| Bit | Meaning |
|-----|---------|
| `0x10` | need_time — daemon should send a timestamp on next reply |
| `0x20` | has_signal_data — signal payload follows in this frame |
| `0x40` | need_bytecode — sensor wants bytecode chunks |
| `0x80` | init_pending — sensor wants the post-pair init frame |

Implementation note: a sensor freshly reset from battery may flood
`f1 ff …` for hours if the daemon does not bring it back to a clean
state. The reinit path in OpenQiara is best-effort and is gated to
trigger only on the literal `f1 ff` byte sequence — receiving routine
heartbeats (`f1 00 …`) must NOT trigger reinit, or you get an infinite
loop where every reinit kicks the sensor back into "need everything".

### 6.5 Daemon reply wflags

When `fbxhome` responds to a PKT frame it builds the outgoing `wflags`
byte cumulatively from `0x80` (ack base) plus per-bit additions driven
by `node->status_flags`. Common end values:

| wflags | Trigger | Extra payload |
|--------|---------|---------------|
| `0x80` | Pure ack — node is calm, nothing pending | none |
| `0x84` | Bytecode chunk continues — bit `0x04` set | 128 B from `get_fw_chunk(node, idx*0x80 + 0x1a, 0x80)` |
| `0xC0` | "More to come" tail — bit `0x40` set when other bits remain | none (the data went elsewhere) |
| `0xC3` | Post-pair init — bit `0x02` (need_init) set | 8 B from `update_init` (`get_fw_chunk(node, 0xc, 0xc)`) |
| `0xC5` | Init pending — bit `0x80` set | call to `FUN_000902b0` (build init payload) |
| **`0xCC`** | **Status response** — bit `0x20` set | **single byte `0x78`** (verified by `mov r1, #0x78` at `0x8b0f8`) |
| `0xCD` | Status + bytecode chunk in same frame | bytecode chunk |

OpenQiara uses `wflags=0xCC, payload=0x78` unconditionally for non-KPD
status acks. This is the most common case and is correct for a node in
the steady "got status, ack it back" path.

What we do not do (deliberately, until we add per-sensor state tracking):

- Track `last_manage` per sensor → STATE frames are not gated on it
- Track `node->status_flags` per sensor → can't pick wflags `0x84`
  vs `0xC3` vs `0xC5` based on which bit was set
- Track `chunk_idx` per sensor → bytecode is sent as a precomputed
  burst (see `internal/domus/bytecode.go`) instead of one chunk per
  reply

---

## 7. Sensor types

| Model prefix | Type | Home Assistant entity |
|-------------|------|-----------------------|
| `HOMELABDWS` | Door/Window | `binary_sensor` (opening) |
| `HOMELABPIR` | Motion | `binary_sensor` (motion) |
| `HOMELABSRN` | Siren | `siren` |
| `HOMELABKPD` | Keypad | `alarm_control_panel` |

The model prefix is read from the beacon during pairing (bytes 15..30 of the
`0x17` beacon, ASCII).

---

## 8. Vendor keys

Sensors are encrypted with vendor-specific 32-byte AES keys. The key is
selected based on the **first 6 bytes** of the beacon (`vendor_prefix`).

Known vendor names (used as identifiers in `/etc/hl/vendors.keys`):

- `cofidur1` ... `cofidur5` — Cofidur EMS, the French OEM that builds Qiara sensors
- `km1`, `km2`, `bkm1`, `bkm2` — additional vendor families

OpenQiara reads `/etc/hl/vendors.keys` at runtime; the file is part of the
stock Qiara rootfs and is **not** redistributed by OpenQiara.

The file format is one key per line:

```
name: base64-encoded-32-bytes
```

---

## 9. Encryption

Sensor data packets use **AES-128-OCB3** authenticated encryption. Key
derivation happens during the `0x1a` / `0x1f` exchange (step 5-6 of the
CTRL handshake): the MCU and the sensor each derive a 16-byte session key
from the vendor key plus random nonces. From that point on, all PKT data
between MCU and sensor is encrypted by the MCU's hardware crypto engine
(`domus_aes_dev`).

**Where encryption happens.** RE of `fbxhome` shows it has no AES OCB
code (`libcrypto.so.3` is linked only for HTTPS to the Free cloud).
The crypto runs entirely **in the EZR32LG MCU firmware**: outbound
managed frames written by the SoC on UART are in cleartext, the MCU
encrypts them just before TX on 868 MHz, and decrypts incoming radio
packets before forwarding cleartext on UART back to the SoC. This is
why OpenQiara can send managed frames without doing any crypto itself.

The per-sensor session key (`node_key`) lives in **the MCU's NVM** and
is invisible to user-space. Removing a sensor's batteries destroys its
copy of the session key; from then on neither the MCU nor any daemon
can re-establish the session — the sensor must be physically
factory-reset (long-press the pairing button) and re-paired to derive
a fresh key.

OpenQiara never sees the plaintext key — the MCU handles encryption
transparently. This means that **OpenQiara cannot impersonate a sensor or
inject fake events**, which is by design.

---

## 10. Glossary

| Term | Meaning |
|------|---------|
| **charmux** | Vendor binary that multiplexes the MCU UART into per-channel UDP sockets |
| **CTRL channel** | UDP ports 8000/8001 — control commands and replies |
| **PKT channel** | UDP ports 8002/8003 — sensor data packets (managed frames) |
| **DomusRF** | The 868 MHz proprietary protocol used by Qiara sensors |
| **Managed frame** | Structured packet format on PKT (varint TLV with flags + wflags + payload) |
| **wflags** | A "wake flag" byte inside managed frames; identifies the message subtype |
| **Signal** | A semantic event from a sensor (state change, temperature, button press). Each sensor type declares 0..10 signals in its bytecode |
| **Bytecode** | A small VM program pushed to a sensor at pairing time, declaring its endpoints, signals, and behaviour |
| **Vendor key** | 32-byte AES master key, one per OEM family, used to derive per-sensor session keys |
| **Cofidur EMS** | The French electronics manufacturer that builds Qiara sensors |
| **fbxhome** | The original Qiara/Free daemon. OpenQiara replaces its application layer but can also coexist with it (proxy mode) |
| **EZR32LG** | Silicon Labs MCU (Cortex-M3 + Si446x radio) inside the camera |
| **uartboot** | Vendor binary that flashes the MCU firmware over UART at every camera boot |

---

## 11. fbxhome binary patch — decoupling KPD from HlAlarm

When `openqiarad` runs in `alarm.mode = alarmo` (Home Assistant Alarmo is
the source of truth), the vendor `fbxhome` daemon still tries to drive
its own internal alarm state machine in parallel: every `KPD_DAY_ALARM`
/ `KPD_NIGHT_ALARM` makes it transition the SRN to
`TIMEOUT_BEFORE_ARMED` and emit an arming beep, independent of what
Alarmo decides. Effects:

- Double pilotage of the SRN (`fbxhome` and `openqiarad` both issuing commands).
- fbxhome's internal arming rules (per-sensor `day_alarm`/`night_alarm`
  flags) can be different from Alarmo's → triggers `fbxhome`'s own wail
  in cases where Alarmo would not have armed at all.
- `fbxhome` calls `reboot_srn` on every `KPD_ALARM_OFF`, causing a
  3–5 s SRN resync.

### What was tried first

All the non-invasive routes turned out to be dead ends:

- `endpoints_write day_alarm=false` / `night_alarm=false` / `alarm_enabled=false` →
  HTTP returns 200 OK with body `{"message":"Not allowed","reason":5}`
  for any session via `create_login_session` (regardless of `acl_group`).
- No fbxbus method to set the alarm status (`alarm_status_get` exists but
  is read-only; no `set_alarm_status` symbol).
- Deleting the `<NodeLink>` entries linking the KPD to HlAlarm in
  `/data/fbxhome.xml`: fbxhome regenerates them at runtime from a static
  vendor descriptor.
- Changing `<Node ... type="Node.HlAlarm" alarm_type="N">` in the XML
  for any value 0..4: accepted by fbxhome but doesn't disable arming.

### The patch

The pilotage actually flows from `HLKpd::event_slot_type::virtual_8` →
HlAlarm via a virtual signal/slot call (`blx r4`), not via the imported
symbol `hls_set_alarm_status`. The handler is a switch on the KPD event
type (0 = `KPD_ALARM_OFF`, 1 = `KPD_DAY_ALARM`, 2 = `KPD_NIGHT_ALARM`,
3 = `KPD_EMERGENCY`, 4 = `KPD_TAMPER`).

We NOP the two `blx r4` instructions that propagate the **arming** cases
(1 and 2) to HlAlarm. The disarm case (0) is left intact. The
`HlKpd: KPD_DAY_ALARM` log line is emitted *before* the NOPed call, so
`openqiarad`'s tail of `/var/log/fbxhome.log` still captures the event
and relays it to Alarmo.

| File offset | Original | Patched | Purpose |
|-------------|----------|---------|---------|
| `0xa4a84` | `34 ff 2f e1` (`blx r4`) | `00 00 a0 e1` (NOP) | case 1 = `KPD_DAY_ALARM` |
| `0xa4aec` | `34 ff 2f e1` (`blx r4`) | `00 00 a0 e1` (NOP) | case 2 = `KPD_NIGHT_ALARM` |

(Validated against fbxhome with MD5 `2fd2a52eb187910176ae81a7432342ef`;
the patched binary's MD5 is `8c89fd04c4f16967cc8900761a464017`.)

### How it's deployed

The patched binary lives on the data partition at
`/data/fbxhome.patched`. `scripts/camera_boot.sh` checks at boot whether
it differs from `/usr/bin/fbxhome`; if so it remounts `/` rw, copies the
patched binary in place, and remounts ro. The original is backed up to
`/usr/bin/fbxhome.orig` (kept on the rootfs).

To revert: `rm /data/fbxhome.patched` (and `cp /usr/bin/fbxhome.orig
/usr/bin/fbxhome` after a remount rw if the patch was already applied
this boot).

## 12. Open questions

These are areas where OpenQiara could be improved by further reverse
engineering. Contributions welcome.

- **Temperature conversion** — opcode `0x0A` byte → °C formula. The conversion
  is done at HTTP serialisation time in fbxhome (a `vldr s15, [obj+944]`
  float load), but the math hasn't been extracted yet.
- **Battery reporting** — `wflags=0x82` has been observed once for PIR low
  battery; we don't know if it's a one-shot warning or part of a periodic
  report. Continuous battery requires the Sigfox cloud API which is dead.
- **Sensor offline detection** — sensors only report on event, so a "sensor
  unreachable" state requires a heartbeat-timeout heuristic. Periods are
  type-dependent: PIR ~6 h, DWS may be longer.
- **Sensor deletion** — removing a sensor from `openqiara.json` does not
  remove it from the MCU NVM. The deletion frame format is unknown.
- **`charmux` replacement** — re-implementing charmux as Go code over
  `/dev/ttyS2` would let OpenQiara run without the vendor binary. ~300 lines,
  no protocol unknowns.
