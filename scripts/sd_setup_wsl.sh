#!/usr/bin/env bash
# Prepare a Qiara camera SD card for OpenQiara — from Windows via WSL2.
#
# Windows has no native ext4 write support, but the WSL2 kernel does — so
# unlike sd_setup.sh/sd_setup_ubuntu.sh (which patch the filesystem offline
# via debugfs, since macOS/some minimal Linux hosts can't mount ext4 at
# all), this variant mounts the partitions directly and edits files with
# plain cp/sed. debugfs is not needed here, and the first version of this
# script (#32) reported that e2fsprogs <1.46 (Ubuntu 20.04's default)
# corrupts directory entries when rm+write-ing a file on a filesystem with
# metadata_csum enabled, destroying /etc/init.d/rcS.real. Not reproduced:
# unverified. It expects the SD card to be attached to the WSL2 VM as a raw
# USB device (not just mounted read-only by Windows).
#
# Prerequisites (one-time, on the Windows host, as admin):
#   winget install usbipd
#   usbipd list                                   # find the BUSID of your SD card reader
#   usbipd bind --busid <BUSID>
#   usbipd attach --wsl --busid <BUSID>
#   usbipd list                                   # Verify that SD card reader is attached to WSL
# Prerequisites (inside WSL2, e.g. Ubuntu or Debian):
#   sudo apt-get update && sudo apt-get install -y e2fsprogs file
#   (file checks that --daemon is an ARM binary; minimal WSL images ship
#   without it. No usbip client is needed in WSL: `usbipd attach --wsl`
#   only exists since usbipd-win 4, which bundles its own. Attached without
#   linux-tools-generic on Debian 13 with usbipd-win 5.3.0, where that
#   package does not even exist.)
#
# After attaching, confirm the device with: lsblk
# WSL2 already uses the first sdX letters for its own virtual disks (system
# image, swap, distro root mounted at /) — none of them has partitions. The
# SD card is the new disk with 3 partitions, e.g. sde with sde1/sde2/sde3.
# IMPORTANT: Windows may auto-mount the partitions under /mnt/wsl or via
# DrvFs — unmount those first with: sudo umount /dev/sdX1 (etc.) if mounted.
#
# Usage:
#   ./scripts/sd_setup_wsl.sh --disk sde --wifi-ssid "MyNetwork" --wifi-pass "MyPassword"
#
# Options:
#   --disk         Linux disk name under /dev (e.g. sde) — REQUIRED
#   --wifi-ssid    WiFi network name — REQUIRED
#   --wifi-pass    WiFi password — REQUIRED
#   --daemon       Path to openqiarad binary (default: bin/openqiarad)
#   --ssh-pubkey   Path to SSH public key to authorize (optional)
#   --dry-run      Show what would be done without writing

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

DISK=""
WIFI_SSID=""
WIFI_PASS=""
DAEMON_BIN="${REPO_ROOT}/bin/openqiarad"
SSH_PUBKEY=""
DRY_RUN=false

# e2fsprogs puts mke2fs in /sbin or /usr/sbin, which is often missing from a
# regular (non-root) user's $PATH on Debian/Ubuntu — `command -v` alone
# would report it as absent even when the package is installed.
find_tool() {
    local name="$1"
    for candidate in "$(command -v "$name" 2>/dev/null)" "/sbin/$name" "/usr/sbin/$name"; do
        if [ -n "$candidate" ] && [ -x "$candidate" ]; then
            echo "$candidate"
            return 0
        fi
    done
    return 1
}

MKE2FS="$(find_tool mke2fs || true)"
if [ -z "$MKE2FS" ]; then
    echo "ERROR: mke2fs not found. Install with: sudo apt-get install -y e2fsprogs"
    exit 1
fi

if ! command -v file >/dev/null 2>&1; then
    echo "ERROR: file not found. Install with: sudo apt-get install -y file"
    exit 1
fi

while [[ $# -gt 0 ]]; do
    case $1 in
        --disk)       DISK="$2"; shift 2;;
        --wifi-ssid)  WIFI_SSID="$2"; shift 2;;
        --wifi-pass)  WIFI_PASS="$2"; shift 2;;
        --daemon)     DAEMON_BIN="$2"; shift 2;;
        --ssh-pubkey) SSH_PUBKEY="$2"; shift 2;;
        --dry-run)    DRY_RUN=true; shift;;
        *) echo "Unknown option: $1"; exit 1;;
    esac
done

if [ -z "$DISK" ] || [ -z "$WIFI_SSID" ] || [ -z "$WIFI_PASS" ]; then
    echo "Usage: $0 --disk <sdX> --wifi-ssid <ssid> --wifi-pass <password>"
    echo ""
    echo "Find your SD card disk with: lsblk"
    exit 1
fi

# The camera authorizes SSH from /data/ssh_authorized_keys and nothing else:
# camera_boot.sh copies it to /root/.ssh/authorized_keys on every boot, and
# there is no password login and no built-in key to fall back on. Flashing a
# card without a key therefore produces a camera nobody can log into — the
# web UI still works, but every SSH attempt returns "Permission denied".
#
# A typo in the path used to be worse than omitting the flag: the write was
# guarded by [ -f "$SSH_PUBKEY" ], so a wrong path was skipped in silence —
# and here the "ssh_authorized_keys ✓" line still printed, claiming success.
# Validate up front, before anything is written to the card.
if [ -n "$SSH_PUBKEY" ]; then
    if [ ! -f "$SSH_PUBKEY" ]; then
        echo "ERROR: --ssh-pubkey $SSH_PUBKEY does not exist"
        echo "Generate one with: ssh-keygen -t ed25519"
        exit 1
    fi
    case "$(cat "$SSH_PUBKEY")" in
        ssh-ed25519\ *|ssh-rsa\ *|ecdsa-sha2-*) ;;
        *)
            echo "ERROR: $SSH_PUBKEY is not an SSH public key"
            echo "Point --ssh-pubkey at the .pub file, not the private key."
            exit 1;;
    esac
elif [ "$DRY_RUN" != true ]; then
    echo "WARNING: no --ssh-pubkey given."
    echo "         The camera will be reachable over HTTP but NOT over SSH,"
    echo "         and the only way to add a key later is to re-flash this card."
    echo "         Pass --ssh-pubkey ~/.ssh/id_ed25519.pub unless you are sure."
    echo ""
    printf "Continue without SSH access? [y/N] "
    read -r REPLY
    case "$REPLY" in
        [Yy]*) ;;
        *) echo "Aborted."; exit 1;;
    esac
fi

# Reject CR/LF in credentials — same rationale as sd_setup.sh (hlconnman
# reads these files line-by-line; other special chars are preserved as-is
# since we write via printf '%s').
for pair in "WIFI_SSID:--wifi-ssid" "WIFI_PASS:--wifi-pass"; do
    var="${pair%%:*}"
    flag="${pair##*:}"
    val="${!var}"
    case "$val" in
        *$'\n'*|*$'\r'*)
            echo "ERROR: $flag contains a newline (invalid for WiFi)"
            exit 1;;
    esac
done

PART_ROOTFS="/dev/${DISK}1"
PART_DATA="/dev/${DISK}2"

CAMERA_BOOT_SH=""
for c in "${REPO_ROOT}/scripts/camera_boot.sh" "$(cd "$(dirname "$0")" && pwd)/camera_boot.sh"; do
    if [ -f "$c" ]; then CAMERA_BOOT_SH="$c"; break; fi
done
if [ -z "$CAMERA_BOOT_SH" ]; then
    echo "ERROR: camera_boot.sh not found next to sd_setup_wsl.sh nor in scripts/"
    exit 1
fi

write_boot_sh() {
    cat "$CAMERA_BOOT_SH" > "$1"
}

if [ ! -b "/dev/${DISK}" ]; then
    # The WSL2 kernel doesn't auto-load usb_storage on attach: the device
    # shows up in lsusb but no /dev/sdX is created until this runs.
    sudo modprobe usb_storage 2>/dev/null || true
fi
if [ ! -b "/dev/${DISK}" ]; then
    echo "ERROR: /dev/${DISK} does not exist (did you 'usbipd attach --wsl'? on Windows, 'usbipd list' must show the reader as Attached; then run 'sudo modprobe usb_storage' and 'lsblk')"
    exit 1
fi
if [ ! -b "$PART_ROOTFS" ] || [ ! -b "$PART_DATA" ]; then
    echo "ERROR: expected partitions ${PART_ROOTFS} and ${PART_DATA} not found"
    echo "Run 'lsblk' and confirm this is the Qiara SD card (3 partitions)."
    exit 1
fi

# Verify this looks like a Qiara SD card (3 Linux partitions) — partition 2
# is about to be reformatted, so having a 1 and a 2 is not proof enough.
PART_COUNT=$(lsblk -no NAME "/dev/${DISK}" | tail -n +2 | wc -l)
if [ "$PART_COUNT" -lt 3 ]; then
    echo "ERROR: Expected 3 partitions on $DISK, found $PART_COUNT"
    echo "Are you sure this is a Qiara camera SD card?"
    exit 1
fi

if [ ! -f "$DAEMON_BIN" ]; then
    echo "ERROR: openqiarad binary not found at $DAEMON_BIN"
    echo "Build it first: cd $(basename "$REPO_ROOT") && make build-arm"
    echo "Or specify path: --daemon /path/to/openqiarad"
    exit 1
fi

DAEMON_ARCH=$(file "$DAEMON_BIN")
if ! echo "$DAEMON_ARCH" | grep -q "ARM"; then
    echo "ERROR: $DAEMON_BIN is not an ARM binary"
    echo "Build with: GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags='-s -w' -o bin/openqiarad ./cmd/openqiarad"
    exit 1
fi

echo "=== OpenQiara SD Setup (WSL2) ==="
echo "Disk:       /dev/$DISK"
echo "WiFi SSID:  $WIFI_SSID"
echo "Daemon:     $DAEMON_BIN ($(du -h "$DAEMON_BIN" | cut -f1))"
[ -n "$SSH_PUBKEY" ] && echo "SSH key:    $SSH_PUBKEY"
echo ""

if [ "$DRY_RUN" = true ]; then
    echo "[DRY RUN] Would write to $DISK. Exiting."
    exit 0
fi

echo "⚠️  This will modify the SD card on /dev/$DISK"
read -p "Continue? [y/N] " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 1
fi

# Make sure Windows/WSL auto-mount didn't grab the partitions
for p in "$PART_ROOTFS" "$PART_DATA"; do
    if mount | grep -q "$p "; then
        echo "Unmounting $p (was auto-mounted)..."
        sudo umount "$p"
    fi
done

MNT_ROOTFS=$(mktemp -d)
MNT_DATA=$(mktemp -d)
cleanup() {
    sudo umount "$MNT_ROOTFS" 2>/dev/null || true
    sudo umount "$MNT_DATA" 2>/dev/null || true
    rmdir "$MNT_ROOTFS" "$MNT_DATA" 2>/dev/null || true
}
trap cleanup EXIT

# --- Step 1: Patch rootfs (partition 1) ---
echo ""
echo "[1/3] Patching rootfs — rcS.real (ext4 journal + OpenQiara autostart)..."

sudo mount "$PART_ROOTFS" "$MNT_ROOTFS"
RCS_REAL="$MNT_ROOTFS/etc/init.d/rcS.real"

# Without this, a missing rcS.real (wrong card) would read as "journal patch
# already present" (grep exits 2 inside the if) and `tee -a` would then create
# a brand-new rcS.real holding nothing but the autostart line.
if [ ! -f "$RCS_REAL" ]; then
    echo "ERROR: /etc/init.d/rcS.real not found on $PART_ROOTFS"
    echo "Are you sure this is a Qiara camera SD card?"
    exit 1
fi

# Same substitution as sd_setup.sh and fix_journal.sh: every
# `tune2fs -O ^has_journal` line, i.e. /data (mmcblk0p2) and /media too.
if grep -q 'tune2fs -O \^has_journal' "$RCS_REAL"; then
    sudo sed -i 's|tune2fs -O \^has_journal|tune2fs -O has_journal|g' "$RCS_REAL"
    sudo sed -i 's|e2fsck -c -y -f /dev/mmcblk0p2|e2fsck -y -f /dev/mmcblk0p2|' "$RCS_REAL"
    echo "  journal-on-/data patch applied ✓"
else
    echo "  journal-on-/data patch already present — skipping"
fi

if grep -q "OpenQiara autostart" "$RCS_REAL"; then
    echo "  autostart patch already present — skipping"
else
    printf '\n\n# === OpenQiara autostart (added by sd_setup_wsl) ===\n[ -x /data/boot.sh ] && /data/boot.sh >> /data/boot_debug.log 2>&1 &\n' | sudo tee -a "$RCS_REAL" >/dev/null
    echo "  autostart patch applied ✓"
fi
sudo chmod 755 "$RCS_REAL"
sudo umount "$MNT_ROOTFS"
echo "  rcS.real written ✓"

# --- Step 2: Write files to /data (partition 2) ---
echo ""
echo "[2/3] Formatting and writing files to /data partition..."

sudo "$MKE2FS" -F -t ext4 -b 4096 -N 128 -m 0 "$PART_DATA" 2>/dev/null
echo "  partition formatted ✓"

sudo mount "$PART_DATA" "$MNT_DATA"

sudo cp "$DAEMON_BIN" "$MNT_DATA/openqiarad"
sudo chmod 755 "$MNT_DATA/openqiarad"
write_boot_sh /tmp/_boot.sh.$$
sudo cp /tmp/_boot.sh.$$ "$MNT_DATA/boot.sh"
rm -f /tmp/_boot.sh.$$
sudo chmod 755 "$MNT_DATA/boot.sh"
printf '%s' "$WIFI_SSID" | sudo tee "$MNT_DATA/wifi_ssid" >/dev/null
printf '%s' "$WIFI_PASS" | sudo tee "$MNT_DATA/wifi_pass" >/dev/null
sudo touch "$MNT_DATA/bridge"

if [ -n "$SSH_PUBKEY" ]; then
    sudo cp "$SSH_PUBKEY" "$MNT_DATA/ssh_authorized_keys"
fi

sync
echo "  openqiarad ✓"
echo "  boot.sh ✓"
echo "  wifi_ssid ✓"
echo "  wifi_pass ✓"
echo "  bridge ✓"
[ -n "$SSH_PUBKEY" ] && echo "  ssh_authorized_keys ✓"

# --- Step 3: Verify ---
echo ""
echo "[3/3] Verifying..."

for f in openqiarad boot.sh wifi_ssid wifi_pass bridge; do
    if [ -e "$MNT_DATA/$f" ]; then
        echo "  /data/$f ✓"
    else
        echo "  /data/$f ✗ MISSING"
    fi
done

sudo umount "$MNT_DATA"

echo ""
echo "=== Done! ==="
echo ""
echo "Next steps:"
echo "  1. On Windows: usbipd detach --busid <BUSID>"
echo "  2. Eject the SD card from Windows and insert it in the camera"
echo "  3. Power on the camera"
echo "  4. Wait ~60s for boot + WiFi connection"
echo "  5. Find the camera:  arp -a"
echo "  6. Open http://<camera-ip>:80"
