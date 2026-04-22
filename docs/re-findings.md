# Reverse Engineering — Findings & Open Questions

This document consolidates everything learned about `fbxhome` (the vendor
daemon) by static analysis of the ARM ELF binary using Ghidra Headless. It
covers the bits that drive `openqiarad`'s charmux mode and explicitly
calls out what's still **unknown** so future work has a starting point.

> See `docs/protocol.md` for the user-facing protocol reference. This file
> is the **research log** behind it.

## 1. Methodology

- Binary: `fbxhome` (ARM EABI5, 32-bit, stripped, dynamically linked)
- Tool: Ghidra 12.0.4 headless analyzer
- Approach: collect string xrefs to known protocol terms ("pairing",
  "DomusAdapter", "HlSrn", …), decompile the resulting functions plus a
  3-deep callee walk; spot-check decompilation against raw ARM asm when
  Ghidra's C output looks suspicious (it does, often).
- Result: 252 functions decompiled around the protocol surface, ~1500 in
  the broader address ranges that contain the radio code.

The repo has no symbols. Class names come from RTTI typeinfo strings
(`5HlDws`, `12DomusAdapter`, …); method names are unrecovered, so all
references below use `FUN_xxxxx` addresses.

## 2. CTRL channel (UDP 8000/8001) — confirmed

### 2.1 Main dispatcher

`FUN_000877f0` is the single switch dispatcher invoked on every byte
received from MCU on CTRL. The opcode is `data[0] & 0x7F`. The
non-trivial cases:

| Opcode | Direction | Meaning | Notes |
|--------|-----------|---------|-------|
| `0x02` | RX | `GET_INFO` reply (8 bytes: opcode, netid_lo, netid_hi, addr, flags×3, **state**) | If `(addr|flags) == 0`, fbxhome immediately sends opcode `0x03` to provision the network |
| `0x05` | RX | `GET_NET` reply | |
| `0x06` | RX | MCU "network ready" — fbxhome replies with `0x05` then `0x02` | |
| `0x07` | RX | `GET_NODES` reply | Header byte + count + per-node entry: `uint32 model_lo`, `uint32 model_hi`, `varint flags` |
| `0x14` | RX | Pairing init received | sets `adapter->pstate = 3` |
| `0x15` | RX | `START_PAIRING` echoed by MCU | sets `pstate = 4` and clears candidate buffers |
| `0x16` | RX | Stop pairing | `pstate = 2` + cleanup |
| `0x17` | RX | **Beacon** from sensor (47 bytes: opcode, vendor_prefix×6, uid×8, model×16, padding) | We extract uid + model, match vendor key |
| `0x18` | RX | Beacon variant — same handler as `0x17` | |
| `0x1b` | RX | Pairing pre-challenge (25 bytes) | |
| `0x1e` | RX | `RESULT` after confirm (13 bytes) | byte 9 = assigned address |
| `0x1f` | RX | `CHALLENGE` (49 bytes) | |

Unhandled cases (`0x08-0x13`, `0x19-0x1a`, `0x1c-0x1d`) fall into empty
`break` — fbxhome accepts them silently or doesn't expect them.

### 2.2 Outbound CTRL opcodes

fbxhome sends these via `FUN_00086508(adapter, opcode_byte)` — single-byte
CTRL commands:

| Opcode | When |
|--------|------|
| `0x02` | `GET_INFO` request (also during periodic retry) |
| `0x05` | `GET_NET` request, also "ack network ready" reply to MCU's `0x06` |
| `0x06` | After adapter reset |
| `0x16` | Stop pairing (timeout from app; `FUN_000866a8` checks `pstate ∈ {3..7}`) |
| `0x1d` | **Reject pairing** during pstate=6 (`FUN_000866e4`) — sent if user aborts after challenge |

Multi-byte CTRL commands constructed inline:

- `[0x03, netid_lo, netid_hi, varint(addr=1), state=0]` (8 bytes) — `SET_NETWORK`. Sent only if `iVar5 == 0` in case `0x02`, i.e. `addr == 0` and `flags == 0` (fresh MCU). Generates a random 16-bit netid with popcount ≥ 4 via `FUN_000806bc`.
- `[0x1a, uid×8, vendor_key×32, padding×16]` (57 bytes) — `PAIR_REQUEST`
- `[0x1c, uid×8]` (9 bytes) — `CONFIRM`
- `[0x15, addr, 0, 0, 0, addr, 0×12]` (18 bytes) — `START_PAIRING`

### 2.3 MCU state byte (last byte of `GET_INFO` reply)

| Value | Observed behaviour |
|-------|--------------------|
| `0x08` | Boot / partial init — only opcodes `0x02-0x07` work |
| `0x09` | Intermediate (we hit this in production after `fbxhome` was disabled) — pairing CTRL works, sensor heartbeat forwarding works |
| `0x0e` | Documented in legacy memory as "post-corruption" after stray opcode `0x03` |
| `0x1d` | Fully initialised — everything works |

The transition `0x08 → 0x1d` happens **only** when `addr == 0`. Once
`addr` is provisioned, fbxhome will not send `0x03` again, so a camera
already at addr=1/state=9 stays at state=9 forever from the daemon's
perspective. This is fine empirically: pairing and PKT both work in
state=9; only some opcodes that require `0x1d` (`SET_KEY = 0x04`)
return error `0x86`.

### 2.4 Adapter pstate (daemon-internal, not on the wire)

`FUN_000864d4(adapter, state)` is the setter; states correspond to
`PSTATE_*` strings:

```
0  unset
1  network init in progress
2  registered, idle
3  pairing init received  (case 0x14)
4  waiting beacon         (case 0x15 echo)
5  vendor key matched     (after beacon → 0x1a sent)
6  challenge sent         (after 0x1f received → 0x1c sent)
7  result received        (case 0x1e → new_node created)
```

`PSTATE_USERCHECK`, `PSTATE_RESET_OBJ`, `PSTATE_FETCH_ITEMID`,
`PSTATE_BAD_TYPE`, `PSTATE_BAD_KEY` etc. exist as additional values on
`FUN_0007fd7c` (the *outer* DomusAdapter pstate, not the inner mux one).
We don't need them in charmux mode because openqiarad doesn't implement
the higher-level provisioning flow.

## 3. PKT channel (UDP 8002/8003) — partially confirmed

### 3.1 Dispatch by `wflags & 0x3F`

`FUN_0008c884` is the per-frame entry. Switches on `wflags & 0x3F`:

| Sub-opcode | Meaning |
|------------|---------|
| `0x01` | **STATE/SIGNAL** frame — events (DWS open/close, PIR motion, KPD button). Requires `last_manage == 0x0c` to be processed; otherwise dropped and `last_manage = 0x0b` |
| `0x02` | **STATUS heartbeat** — calls `FUN_0008b3c4` → `parse_status` → updates `node->status_flags` |
| `0x0d` | Some completion / ack flow — calls `vtable[0x58]`, then `last_manage = 0x0d` |

After the per-type processing, every frame ends with a call to
`FUN_0008af94` ("more manage packet needed") which builds the daemon's
reply.

### 3.2 `parse_status(node, flags_varint)` — `FUN_00089e78`

Reads the status varint from the heartbeat payload:

| Bit | Meaning | Action |
|-----|---------|--------|
| `0x10` | need_time | sets `node->status_flags |= 1` |
| `0x40` | need_bytecode | sets `node->status_flags |= 0x60` (bits 5+6) |

Other bits exist (`0x20` = has_signal_data, `0x80` = init_pending) and
are read in the calling function `FUN_0008b3c4`, not in `parse_status`
itself.

### 3.3 Reply builder `FUN_0008af94` — verified against raw asm

The byte at `[sp,#0xe]` accumulates the wflags of the outgoing frame.
Initial value `0x80` (ack base). Subsequent bits are OR'd in based on
`node->status_flags`:

| `node->status_flags` bit | wflags OR | extra payload | side effect |
|-----|-----|-----|-----|
| `0x80` (init_pending) | `0x05` (= ack + bit2 + bit0) | calls `FUN_000902b0` (build init payload) | `last_manage = 0x0d` |
| `0x20` (has_signal) | `0x4C` (= bits 6+3+2) | **`push_back(0x78)`** (verified by `mov r1, #0x78` at `0x8b0f8`) | `last_manage = 0x0c` |
| `0x02` (need_init) | `0x43` (= ack + 0x40 + 0x03) | calls `FUN_0008ad80` (`update_init`) → 8 bytes from `get_fw_chunk` | clears bit 0x02, sets bit 0x04 |
| `0x04` (chunk_continue) | `0x04` | calls `FUN_0008ac58` (`get_chunk`) → 128 bytes from `get_fw_chunk(node, chunk_idx*0x80 + 0x1a, 0x80)` | increments `node->chunk_idx` (`+0x170`) |
| `0x40` (need_bytecode_pending) | `0x40` | calls `vtable[0x58]` (sensor-specific) | `last_manage = 0x0d`, clears bit 0x40 |
| any non-zero remaining | `0x40` | — | sets `local_58->status_flags |= 1` ("more pending") |

So the canonical "status keep-alive" reply is `wflags=0xCC, payload=0x78`
(when bits 0x20 set, then later 0x40 set, 0x80 + 0x4C + 0x40 = 0xCC).
**Our hardcoded `wflags=0xCC payload=0x78` is correct for this path.**

What we *don't* do that fbxhome does:

- Track `node->status_flags` per sensor → can't choose wflags `0x84`,
  `0x43`, `0x05`, etc. when the sensor is in a different state.
- Track `last_manage` per sensor → STATE frames are dispatched by us
  even when fbxhome would drop them (`last_manage != 0x0c`).
- Track `chunk_idx` per sensor → we send the entire bytecode in one
  reinit burst instead of one chunk per heartbeat reply.

These are correctness-affecting in degraded states (sensor stuck in
`f1 ff` post-battery loop) but harmless in nominal operation.

### 3.4 Heartbeat payload `f1 XX YY` — varint flags, not opcodes

A widespread misreading in our code: the bytes `f1 00 01`, `f1 db 01`,
`f1 ff 01` look like fixed messages but are actually **a single LEB128
varint**:

```
f1 00 01 → 0x71 = 0b1110001        — bits 0,4,5,6 set
f1 db 01 → 0x6df1                  — many bits
f1 ff 01 → 0x7ff1                  — almost all low bits
```

After parsing, the varint is fed to `parse_status` which only cares about
bits `0x10` and `0x40` (and the caller `FUN_0008b3c4` cares about `0x20`,
`0x80`, etc). So the apparent "states" `00`, `db`, `ff` are not states —
they're just different bitmaps of the same status flags. A sensor freshly
booted from battery floods `f1 ff` because **everything** is "pending
sync"; once the daemon catches it up the bits clear and you'd see
`f1 00`.

`internal/camera/charmux_client.go::detectReinitHeartbeat` was triggering
on **any** byte equal to `0xf1`, which is why receiving routine
heartbeats kept rebooting our reinit loop. Fixed — now only `f1 ff`
triggers reinit.

## 4. Sensors — class hierarchy (from RTTI)

```
DomusNode  (base, 0x12DomusNode RTTI — vtable @ 0x80a3c xref)
├── HlDws  (0x5HlDws  — vtable @ 0x118604, 14 main slots + secondary RTTI)
├── HlPir  (0x5HlPir)
├── HLKpd  (0x5HLKpd)
└── HlSrn  (0x5HlSrn)
```

The vtable layout we mostly care about:

| vtable offset | Purpose (inferred) |
|---|---|
| `+0x00` | dtor1 |
| `+0x04` | dtor2 |
| `+0x08-0x14` | misc (clone? equality?) |
| `+0x18` | **on_signal** — for HlDws, `FUN_000add7c` stores into a `signal_id → handler` Rb-tree |
| `+0x44` | (read in `FUN_0008af94` via `vtable[0x44]`?) |
| `+0x48` | called after PKT processing (`FUN_0008b3c4` line `(*vtable[0x48])`) |
| `+0x4C` | called when `node->status_flags & 8` set (during status processing) |
| `+0x50` | dispatch a varint argument (signal? — from `FUN_0008b3c4` line `(*vtable[0x50])(node, varint)`) |
| `+0x58` | "build bytecode response" for need_bytecode path |

Functions with offsets ≥ `+0x40` in the dump appear as `FUN_0011xxxx` —
those are **secondary RTTI thunks** for multiple inheritance, not real
methods. Ghidra's decompiler can't follow them cleanly without symbol
files.

### 4.1 Bytecode chunking (fbxhome vs OpenQiara)

`FUN_0008ac58` (get_fw_chunk wrapper) shows fbxhome sends bytecode
chunks of **exactly 128 bytes** at offset `chunk_idx * 0x80 + 0x1a`,
once per `more_manage_packet_needed` reply. The local payload includes
a 2-byte chunk-index prefix `(chunk_idx * 0x80) >> 4` (a 16-bit
identifier).

Our code in `internal/domus/bytecode.go` ships **pre-captured chunks**
of 18-156 bytes, harvested from a tcpdump of a working pairing. They
work because the sensor's bytecode loader looks at the chunk header,
not the framing — but our chunks are not 1-to-1 with what fbxhome
generates today. This is fine for the pairing flow but means we cannot
implement per-sensor `chunk_idx` tracking without re-deriving the
chunks at fbxhome's granularity.

### 4.2 Sensor `send_config` (per-type)

| Sensor | Function | Format |
|--------|----------|--------|
| HlDws | `FUN_000a8a88` | byte 0 = 1 + 3 push_backs of `(char)param_2` (a pointer cast — actual value lost in decompilation) |
| HlPir | `FUN_000aebd8` | identical to HlDws |
| HLKpd | `FUN_000b4418` | (not deeply analysed) |
| HlSrn | `FUN_000bc574` (`get_state_sent`) | byte 0 = 1, two push_backs, then a state machine update (state 1; if was state 6 sets a flag at +0x474) |

Our static config payloads (`55 00 00 00 00 00 00` for DWS/PIR,
`55 06` for SRN, `55 00 02 / 55 00 04 hash` for KPD) come from tcpdump
captures, not from the RE — the decompilation is too pointer-cast-mangled
to recover them statically.

## 5. Sensor SF format — partial

`FUN_000b2054` (HlPir-side parser, 175 lines) handles **SF (Signal
Frame) messages**:

```
byte 0 = sf_msg_type (varint)
byte 1+ = sub_type (varint)
case 1: 4-byte data + varint (top 6 bits used: << 0x12 >> 0x18) + uint8
case 7: 1 varint
case 8: empty
case 10: 1 varint (logged)
```

These are encoded inside the PKT data block when bit `0x40` of the
status flags is set. They carry battery percentage, temperature,
firmware version, etc. We don't currently parse them — that's why
`battery=0, temperature=0` everywhere in OpenQiara. Adding a parser
here is the path to real telemetry without round-tripping fbxhome.

## 6. What we did NOT find / verify

This is the explicit list of **unknowns**, written down so the next
person doesn't have to re-do the dead ends.

### 6.1 Crypto

- The on-the-wire DomusRF radio frames are encrypted (AES-128-OCB3
  per legacy memory). `fbxhome` references `libcrypto.so.3` only for
  HTTPS — there's no AES OCB code in the binary itself. **Conclusion:
  the radio crypto is in the EZR32LG MCU firmware**, not in
  `fbxhome`. The MCU encrypts on TX, decrypts on RX, the SoC sees
  cleartext on UART. This is consistent with our observation that
  OpenQiara sends managed frames in the clear and they work.
- `vendors.keys` is a list of **vendor public keys** used during the
  pairing handshake (`0x1a` PAIR_REQUEST embeds one). Not the
  per-sensor session key.
- `node_key` (per-sensor session key) is established during pairing
  and stored **in the MCU's NVM**. Daemons never see it. This is
  also why a battery-removed sensor can't be recovered without a
  factory reset — the sensor side has lost its half of the session
  key.

### 6.2 The `f1 ff` recovery problem

**The biggest open question.** The PIR (id=8) and DWS (id=3) on the
test camera have been emitting `f1 ff 01` heartbeats every ~1.5s for
hours, with no transition back to `f1 00 01` (normal). Our reinit
flow (bytecode + config + watchdog NVM commit) does not break the
loop. The legacy memory says:

> Workaround fiable : factory reset (bouton 10s) + re-pair via l'UI.

This is consistent with the RE: nothing in `fbxhome` implements an
"unstuck-sensor" command either. The intended way to reset a sensor's
state is via its physical button. Software-only recovery would
require:

1. Discovering an MCU command to forcibly clear/reset a sensor's
   pairing entry (not present in the strings or the dispatcher).
2. Or replaying enough of `fbxhome`'s post-pair init sequence
   (`update_init` from `FUN_0008ad80`) to re-establish the session
   key — but this requires knowing the actual `update_init` payload,
   which we don't.

**Open data**: we have the pcap of a successful DWS pairing
(`our_pairing.pcap`) but it only covers the CTRL handshake, not the
post-pair PKT dialog. A capture of fbxhome handling a
post-battery-removed sensor would be a goldmine.

### 6.3 The state=9 mystery

We observe `MCU state byte = 9` on the production camera. The protocol
docs only enumerate `0x08` (boot) and `0x1d` (ready). State `9` is
between them, and pairing CTRL works in this state — but we don't know
which bits exactly are set. A more thorough RE of the **MCU firmware**
(`/lib/firmwares/hlcam02_ctrl.bin`) would clarify, but we never
reverse-engineered that binary.

### 6.4 Multiple inheritance vtables

DomusNode is the base of 4 sensor classes plus inherits from
`fbxsignals` (signal/slot framework). The vtables we dumped have a
"primary" set of 14-15 slots (offsets 0x00-0x38) followed by what
looks like a secondary RTTI block. Calls to `vtable[0x4C]`, `[0x50]`,
`[0x58]` are inside this secondary block and Ghidra's decompiler
gives garbled output (data bytes interpreted as code, "Could not
recover jumptable" warnings). Reading the corresponding ARM asm
manually showed they are **adjuster thunks** that fix `this` then
jump to a real method elsewhere — but tracing each one is a manual
~30min job per slot.

This is why we don't know exactly what `vtable[0x58]` does for HlDws
versus HlSrn, beyond "it builds a wflags response when need_bytecode
is set".

### 6.5 PKT router `FUN_0008cc60`

This is the function that decides **which** node to dispatch a PKT
event to, based on the frame's `src/dst/flags` fields. We did not
fully understand it. Our code dispatches by `addr` only (which works
for our test cases) but there are paths in `FUN_0008cc60` that walk
linked lists and check signal-class matches — suggesting fbxhome can
route a single frame to multiple node-vm objects, which we don't.

### 6.6 Periodic retry on GetInfo / GetNet

`FUN_00086800` retries `0x02` then `0x05` if `pstate == 0` or `1`. It
is called by something we did not identify (timer? signal slot
trigger?). OpenQiara retries 3 times at startup but never again — if
the MCU comes back later, we don't notice until the next manual
restart. Adding a 30s background retry would be straightforward but
hasn't been done.

### 6.7 Counters

OpenQiara uses one global 32-bit counter for all outgoing managed
frames (`CharmuxClient.manageCnt`). fbxhome appears to use a
**per-node** counter (`chunk_idx` in `node + 0x170`, plus separate
fields for the manage cycle). Empirically our global counter works,
but it's possible some sensors validate counter contiguity and reject
gaps when other sensors' acks bump the global value between two
consecutive frames to the same sensor.

## 6.11 fbxhome `-U 1` flag = use local update_manifest (2026-05-13)

When `fbxhome` runs without the cloud (post-Free shutdown), the
`UpdateManager::fetch` HTTP call to `<x>.srv.home-labs.fr/update_manifest`
fails silently. Without that manifest, fbxhome never sets `target_fw_fnv`
on freshly paired nodes, so bytecode is never pushed and the sensor
stays in a "paired but not operational" state — battery and temperature
readable, but no open/close events.

The vendor binary has a `-U 1` command-line flag advertised in its usage
string (`: 1 means use local update_manifest`). With this flag, fbxhome
loads `/etc/hl/update_manifest.json` (via `/data/update_manifest.json`
which is a symlink) instead of fetching from cloud. The shipped manifest
already declares the four sensor types and their `hash_image` values that
match the bytecode files in `/data/firmwares/`.

`scripts/camera_boot.sh` should be updated to pass `-U 1` when launching
fbxhome via `fbxupstart`, or fbxhome should be started manually with
that flag.

Verified via log:
```
fbxhome: UpdateManager::load_local_update_conf()
fbxhome: FwDownloader::process_fw fw HOMELABDWS00ACFD_48654bf742f5070e.bin already exists valid = 1
fbxhome: _target_fw_fnv 48654bf742f5070e
```

This bypasses the cloud dependency entirely — no mock HTTP server
needed.

## 6.10 Fingerprint submission for fbxhome pairing (2026-05-13)

fbxhome's pairing flow walks through a series of UI layouts. The first
one after `start_adapter` is `QRCode` — fbxhome refuses to advance until
it gets the 16-char hex fingerprint of the sensor (the prefix of the QR
code printed on the device label). Our initial `mode=fbxhome` cut sent
`{op:"poll"}` repeatedly and got nowhere because fbxhome was stuck
waiting for the fingerprint.

Fix: `StartPairing` now takes a fingerprint, stores it in
`fingerprints[session]`. `PollPairing` checks if `LayoutName == "QRCode"`
on the response and, if so, sends `{op:"next", fields:[fingerprint]}`
to unblock fbxhome. After that, fbxhome cycles through `RemoveTab`
("alimenter l'objet en retirant la languette de la pile") then
`WaitUserCheck` then `Terminated` with the new node ID.

The web UI now exposes a fingerprint input on the pairing page. Format
checked client-side: `[0-9a-f]{16}` (16 lowercase hex). Known
fingerprints from memory:
- DWS: `cca878cd4128e306`
- PIR: `e27857ed18f5d2fb`
- SRN: `5b4786e1891e67b1`

Verified pairing flow 2026-05-13: DWS got node ID 14, paired in ~25s
through fbxhome end-to-end, no manual RE-driven CTRL handshake.

## 6.9 Switch to mode=fbxhome (2026-05-13)

After ~11 days of charmux mode in production, the conclusion is that
charmux works for **the happy path** (a sensor freshly paired stays
fine for hours), but every sensor eventually trips the post-battery
`f1 ff` loop (KPD on day 4, DWS on day 1, PIR before pairing). Our
RE-driven fix (bytecode chunk reply on bit 0x40) did not help —
empirically the sensor rejects whatever we send. The chunks shipped
to KPD over 3000+ heartbeats never broke the loop.

**Decision**: keep the daemon, ship it in `mode=fbxhome`. The vendor
binary `fbxhome` runs locally (no cloud needed — `fbxbusctl call
fbxhome create_login_session 1 1` issues a valid session token from
fbxbus). It owns charmux, handles pairing, bytecode flow and
post-battery recovery — all the things we couldn't fully reverse.

Architecture in fbxhome mode:

```
sensors (radio) ──── MCU ──── charmux ──── fbxhome (vendor)
                                              │
                                   HTTPS :64218 (api/v1/home/*)
                                   HTTP  :10000 (rpc/*)
                                              │
                                          openqiarad
                                              │
                                  MQTT, HomeKit, web UI
```

`internal/camera/fbxhome.go` now uses two base URLs:
- `privURL = http://[::1]:10000` for `/rpc/get_domus_nodes` and similar
- `homeURL = https://[::1]:64218` for `/api/v1/home/*` (pairing, endpoints_read/write)

Self-signed cert on 64218, TLS verify is skipped. Both endpoints accept
the same `X-Hlcore-Session-Id` token. Polling on a 2s interval picks up
sensor state changes and emits them on the events channel — identical
external behaviour to charmux mode, but without us being on the radio
hot path.

`scripts/camera_boot.sh` now starts fbxhome (instead of stopping it)
and launches openqiarad with `-mode fbxhome`.

This trades the open-radio-stack purity for a system that actually
stays up. The charmux mode code stays in tree for users who want it
or who want to extend the RE further (see section 8 for the list of
things still to try if anyone wants to revive it).

## 6.8 Sensor battery drain via unanswered need_bytecode (2026-05-09)

A 6-day production run revealed a new failure mode that the original RE
hinted at but had not connected to battery life:

- **DWS id=12**: paired 2026-05-02 16:03, last sensor event 18:08 same day,
  then 7589 raw heartbeats over the next ~17h ending 2026-05-03 11:29:56.
  Frequency: ~1 frame every 4 seconds (measured 16 frames/min steady,
  decaying to 13/10/4/0 in the final 5 minutes — classic battery-flat
  pattern).
- **KPD id=7**: same period, only 24 frames spaced ~12h apart. KPD never
  asks for bytecode (uses FNV announce path with `wflags=0x81`).
- **SRN id=11**: silent over the whole window.

Decoded one DWS heartbeat: `010cff8298670c830082f10001` = manage frame
with `WFlags=0x82` and payload `f1 00 01`. The payload is a single LEB128
varint = `0x71` = bits `0x10|0x20|0x40` = `need_time | has_signal |
**need_bytecode**`.

Our code's response path for this is `wflags=0xCC payload=0x78` (status
ack), exactly the path described in section 3.5 — but section 3.5's table
also shows that when bit `0x40` is set, fbxhome sends a *bytecode chunk*
in addition (or instead). Without that, the sensor never gets the
bytecode it asked for and re-emits the heartbeat ~every 4s instead of
its normal ~12h cycle. **At ~10000× the expected radio TX rate, even
fresh batteries die in roughly 24 hours.**

This is the root cause of the recurring "sensor died after a day" reports
that we kept treating as battery wear. The battery wear is real, but it
is induced by our software not satisfying the sensor's bytecode request.

**Fix shipped 2026-05-09**:
`charmux_client.go::shouldSendBytecodeChunk` + `sendBytecodeChunk`. When
a status heartbeat arrives with bit `0x40` set in its varint flags, push
one chunk from the pre-captured bytecode list (advancing
`c.chunkIdx[addr]` per heartbeat) instead of the standard ack. Reuses
the same chunks `internal/domus/bytecode.go` ships for the pairing flow.
Wflags on the outgoing chunk = `0xCD` to match the chunk-continue path
in fbxhome. Skipping `c.ackPKTEvent` when we send a chunk avoids
double-replying.

This is *minimalist* compared to fbxhome, which tracks `node->status_flags`
properly and only marks chunks as sent when the sensor confirms — but it
should be enough to break the heartbeat flood and let sensors return to
their normal sleep cycle. To validate: replace the DWS battery, watch
the heartbeat rate drop from ~1/4s to ~1/12h within a few minutes.

## 7. Bugs we fixed in OpenQiara as a result of this RE

The specific changes that are tied to RE findings, not generic
hygiene:

1. `detectReinitHeartbeat` was matching **any** `0xf1` byte in
   payload offsets 8-11. A stuck-in-`f1 ff` sensor would emit
   correctly, our code would respond with a full reinit (bytecode +
   config + NVM commit), the sensor would re-emit `f1 ff` because
   the reinit jolt put it back in "need everything" state, and we'd
   fire another reinit 30s later. This was the ground-truth root
   cause of the recurring "DWS just died after I touched it"
   complaints. Fix: match `f1 ff` only.

2. Reinit cooldown for DWS/PIR was 30 seconds based on a comment
   saying "DWS/PIR only transmit reinit heartbeats for ~60s after
   battery swap then go silent". Empirically this is **wrong** —
   they transmit for hours. Bumped to 5 minutes; reinit is best-effort
   anyway, and pounding on a stuck sensor every 30s only burns CPU
   and risks more NVM wear.

3. Initial GetInfo/GetNet had no retry. Added 3 retries with logging
   at INFO instead of WARN for the intermediate failures. Still
   doesn't help if the MCU is permanently in state=9, but cleans up
   the boot logs in the normal warm-restart case.

4. `Sensors()` was firing a fresh `GetNodes` on every UI poll. Added
   a 5s cache. Memory `feedback_ctrl_calls_block_pkt.md` warned that
   CTRL calls can break PKT dialogs; this was real and was probably
   contributing to flaky pairings.

5. `buildSensorList` only merged the live cache for sensors already
   in the persisted config. A freshly paired sensor (only in
   `c.sensors` until the user polls `/api/sensors/pair?session=N`) was
   invisible until its config write. Added a live-cache-first merge.

## 8. Things to test, in order of likely value

1. Replay the pre-captured DWS bytecode chunks to a freshly factory-reset
   DWS while sniffing port 8003: confirm chunk format is still valid in
   2026 firmware. (We last validated this on a 2026-04 capture.)
2. Capture the **fbxhome post-battery-removed-sensor** flow with tcpdump
   on ports 8000-8003. This is the missing piece for `f1 ff` recovery.
3. Implement an SF-message parser (section 5) and see if battery and
   temperature start showing up in the UI.
4. Try `vtable[0x58]` of HlDws by setting up a controlled frame and
   observing the response — manually trace the ARM asm to figure out
   what it builds.
