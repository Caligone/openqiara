#!/bin/sh
# Re-enable the ext4 journal on /data (and /media) for an EXISTING OpenQiara
# camera — the power-cut resilience fix, applied in place over SSH.
#
# Why: stock /etc/init.d/rcS.real strips the journal from /data on every boot
# (`tune2fs -O ^has_journal /dev/mmcblk0p2`) and then runs `e2fsck -c -y -f`.
# A journal-less ext4 does not survive a mains outage mid-write: the fs
# corrupts and `e2fsck -y` can gut /data entirely, wiping the whole install
# (observed 2026-08). This script patches rcS.real so the journal is kept, and
# creates the journal now so protection is effective immediately — no reinstall.
#
# New installs via sd_setup.sh already include this; run this only on cameras
# set up before the fix.
#
# Usage (ON THE CAMERA):
#   sh /tmp/fix_journal.sh
#
# Idempotent: safe to run twice. Reversible: rcS.real is backed up first.

set -eu

RCS=/etc/init.d/rcS.real
BACKUP=/data/rcS.real.bak-journal
DATA_DEV=/dev/mmcblk0p2
MEDIA_DEV=/dev/mmcblk0p3

log() { echo "[fix_journal] $*"; }

[ -f "$RCS" ] || { echo "[fix_journal] ERROR: $RCS not found — run on the camera." >&2; exit 1; }

# --- 1. Patch rcS.real so the journal is kept across future boots ---
if grep -q 'tune2fs -O \^has_journal' "$RCS"; then
    [ -f "$BACKUP" ] || cp -p "$RCS" "$BACKUP"
    log "backed up rcS.real -> $BACKUP"

    # rootfs is mounted read-only; remount rw for the edit, then back to ro.
    remounted=false
    if mount | grep -q ' / .*[( ]ro[,)]'; then
        mount -o remount,rw /
        remounted=true
    fi

    sed -i 's|tune2fs -O \^has_journal|tune2fs -O has_journal|g' "$RCS"
    # Drop the destructive per-boot badblocks scan (-c) on /data.
    sed -i 's|e2fsck -c -y -f /dev/mmcblk0p2|e2fsck -y -f /dev/mmcblk0p2|' "$RCS"
    sync

    [ "$remounted" = true ] && mount -o remount,ro /
    log "rcS.real patched (journal kept on future boots)"
else
    log "rcS.real already patched — skipping"
fi

# --- 2. Create the journal now (effective immediately, no reboot needed) ---
# tune2fs -O has_journal works on a mounted ext4: it adds the journal inode.
for dev in "$DATA_DEV" "$MEDIA_DEV"; do
    if tune2fs -l "$dev" 2>/dev/null | grep -q has_journal; then
        log "$dev already has a journal"
    else
        tune2fs -O has_journal "$dev"
        log "$dev journal created"
    fi
done

log "done. /data and /media now survive a power cut (ext4 replays the journal)."
