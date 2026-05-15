#!/usr/bin/env bash
# Patch fbxhome to decouple the KPD events from the internal alarm
# state machine, so openqiarad can drive the alarm without fbxhome
# double-piloting the SRN.
#
# Runs ON THE CAMERA (after SSH access). Reads /usr/bin/fbxhome,
# verifies its MD5 matches a known-good vendor binary, NOPs two
# `blx r4` instructions at file offsets 0xa4a84 and 0xa4aec, writes
# the result to /data/fbxhome.patched.
#
# camera_boot.sh picks up /data/fbxhome.patched on next boot and
# installs it in place (remount,rw → cp → remount,ro).
#
# What the patch does: see docs/protocol.md § "fbxhome binary patch".
#
# Usage (on the camera):
#   ./patch_fbxhome.sh

set -euo pipefail

VENDOR_BIN="/usr/bin/fbxhome"
OUT_FILE="/data/fbxhome.patched"

# Known vendor binary MD5 (firmware version validated 2026-05-15).
# If the user's MD5 differs the offsets may have shifted — abort
# rather than corrupt the binary.
EXPECTED_MD5="2fd2a52eb187910176ae81a7432342ef"

# Patch offsets and instructions. ARM little-endian.
#   0xa4a84: blx r4 (34 ff 2f e1) → NOP (00 00 a0 e1)   — case 1 KPD_DAY_ALARM
#   0xa4aec: blx r4 (34 ff 2f e1) → NOP (00 00 a0 e1)   — case 2 KPD_NIGHT_ALARM
OFFSETS=("0xa4a84" "0xa4aec")
ORIG_BYTES='\x34\xff\x2f\xe1'
NOP_BYTES='\x00\x00\xa0\xe1'

log() { echo "[patch_fbxhome] $*"; }
die() { echo "[patch_fbxhome] ERROR: $*" >&2; exit 1; }

[ -r "$VENDOR_BIN" ] || die "$VENDOR_BIN not readable. Run on the camera."

ACTUAL_MD5=$(md5sum "$VENDOR_BIN" | awk '{print $1}')
if [ "$ACTUAL_MD5" != "$EXPECTED_MD5" ]; then
    die "MD5 mismatch — got $ACTUAL_MD5, expected $EXPECTED_MD5.
This script only knows the offsets for the firmware shipped with
the cameras tested by the author. A different firmware may need
different offsets; abort rather than corrupt your binary."
fi
log "vendor binary verified ($ACTUAL_MD5)"

# Copy vendor → output, then patch in place. The vendor binary is
# only 1 MB so a full copy is trivial.
cp "$VENDOR_BIN" "$OUT_FILE"

for off in "${OFFSETS[@]}"; do
    # Verify the original bytes before NOPing — defense in depth.
    actual=$(dd if="$OUT_FILE" bs=1 skip=$((off)) count=4 status=none | xxd -p)
    if [ "$actual" != "34ff2fe1" ]; then
        die "offset $off: expected 34ff2fe1, got $actual. Aborting."
    fi
    # printf into dd is the portable way to do a binary patch from busybox.
    printf "$NOP_BYTES" | dd of="$OUT_FILE" bs=1 seek=$((off)) count=4 conv=notrunc status=none
    log "patched $off"
done

PATCHED_MD5=$(md5sum "$OUT_FILE" | awk '{print $1}')
log "patched binary written to $OUT_FILE (md5: $PATCHED_MD5)"
log "expected md5: 8c89fd04c4f16967cc8900761a464017"

if [ "$PATCHED_MD5" != "8c89fd04c4f16967cc8900761a464017" ]; then
    log "WARNING: patched md5 doesn't match the reference. The patch was
    applied but you should investigate before rebooting."
    exit 2
fi

chmod 755 "$OUT_FILE"
log "done. Reboot the camera (or restart fbxhome) to apply the patch."
