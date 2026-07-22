#!/usr/bin/env bash

# Prepare a Qiara camera SD card for OpenQiara — entirely offline from Linux (Ubuntu/Debian).
# Adapted from the original macOS version (scripts/sd_setup.sh).
#
# Prerequisites:
# - Ubuntu/Debian with e2fsprogs installed (usually preinstalled; otherwise: sudo apt install e2fsprogs)
# - The SD card from a stock Qiara camera inserted in the machine
# - A pre-built openqiarad binary (run: make build-arm)
#
# Usage:
#   sudo ./scripts/sd_setup_ubuntu.sh --disk sdb --wifi-ssid "MyNetwork" --wifi-pass "MyPassword"
#   sudo ./scripts/sd_setup_ubuntu.sh --disk mmcblk0 --wifi-ssid "MyNetwork" --wifi-pass "MyPassword"
#
# Options:
#   --disk         Linux block device name without /dev/ (e.g. sdb, mmcblk0) — REQUIRED
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

# --- Locate e2fsprogs tools ---
# On Ubuntu/Debian these are normally already on PATH (or in /sbin), unlike
# the Homebrew install on macOS which hides them under a keg prefix.
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

DEBUGFS="$(find_tool debugfs || true)"
if [ -z "$DEBUGFS" ]; then
    echo "ERROR: debugfs not found. Install with: sudo apt install e2fsprogs"
    exit 1
fi

MKE2FS="$(find_tool mke2fs || true)"
if [ -z "$MKE2FS" ]; then
    echo "ERROR: mke2fs not found. Install with: sudo apt install e2fsprogs"
    exit 1
fi

while [[ $# -gt 0 ]]; do
    case $1 in
        --disk) DISK="$2"; shift 2;;
        --wifi-ssid) WIFI_SSID="$2"; shift 2;;
        --wifi-pass) WIFI_PASS="$2"; shift 2;;
        --daemon) DAEMON_BIN="$2"; shift 2;;
        --ssh-pubkey) SSH_PUBKEY="$2"; shift 2;;
        --dry-run) DRY_RUN=true; shift;;
        *) echo "Unknown option: $1"; exit 1;;
    esac
done

if [ -z "$DISK" ] || [ -z "$WIFI_SSID" ] || [ -z "$WIFI_PASS" ]; then
    echo "Usage: $0 --disk <device> --wifi-ssid <ssid> --wifi-pass <password>"
    echo ""
    echo "Find your SD card device with: lsblk"
    exit 1
fi

# Reject CR/LF in credentials — invalid in any WiFi network and would
# silently truncate when hlconnman reads the files line-by-line.
# Other special chars ($ " \ space accents emoji) are preserved as-is because
# we write the files via printf '%s', not echo or shell interpolation.
# (NUL bytes can't appear: bash strips them at argv parsing time.)
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

# --- Partition naming ---
# Linux block devices use different partition-suffix conventions depending
# on the device family:
#   - SCSI/USB card readers:  /dev/sdb  -> /dev/sdb1,  /dev/sdb2
#   - eMMC/SD host controller: /dev/mmcblk0 -> /dev/mmcblk0p1, /dev/mmcblk0p2
#   - NVMe (not expected here): /dev/nvme0n1 -> /dev/nvme0n1p1
# This mirrors the macOS diskNs1/diskNs2 convention used in the original script.
if [[ "$DISK" =~ (mmcblk|nvme) ]]; then
    PART_SUFFIX="p"
else
    PART_SUFFIX=""
fi
PART_ROOTFS="/dev/${DISK}${PART_SUFFIX}1"
PART_DATA="/dev/${DISK}${PART_SUFFIX}2"

# write_boot_sh <dest> — copies scripts/camera_boot.sh (the single source of
# truth for /data/boot.sh) to <dest>. Located either in the repo checkout
# (scripts/camera_boot.sh) or next to this script for a release-only install
# (the release publishes camera_boot.sh alongside sd_setup.sh).
CAMERA_BOOT_SH=""
for c in "${REPO_ROOT}/scripts/camera_boot.sh" "$(cd "$(dirname "$0")" && pwd)/camera_boot.sh"; do
    if [ -f "$c" ]; then CAMERA_BOOT_SH="$c"; break; fi
done
if [ -z "$CAMERA_BOOT_SH" ]; then
    echo "ERROR: camera_boot.sh not found next to sd_setup.sh nor in scripts/"
    echo "In a release-only install, download it too:"
    echo "  curl -LO \$BASE/camera_boot.sh"
    exit 1
fi

write_boot_sh() {
    cat "$CAMERA_BOOT_SH" > "$1"
}

# --- Sanity checks ---
if [ ! -b "/dev/${DISK}" ]; then
    echo "ERROR: /dev/${DISK} does not exist"
    exit 1
fi

# Verify this looks like a Qiara SD card (3 Linux partitions).
# lsblk replaces macOS's `diskutil list` here.
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

# Verify the binary is ARM
DAEMON_ARCH=$(file "$DAEMON_BIN")
if ! echo "$DAEMON_ARCH" | grep -q "ARM"; then
    echo "ERROR: $DAEMON_BIN is not an ARM binary"
    echo "Build with: GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags='-s -w' -o bin/openqiarad ./cmd/openqiarad"
    exit 1
fi

echo "=== OpenQiara SD Setup ==="
echo "Disk: /dev/$DISK"
echo "WiFi SSID: $WIFI_SSID"
echo "Daemon: $DAEMON_BIN ($(du -h "$DAEMON_BIN" | cut -f1))"
[ -n "$SSH_PUBKEY" ] && echo "SSH key: $SSH_PUBKEY"
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

# On Linux, udev normally grants block-device access to the `disk` group
# and this script is expected to run as root (sudo), so there is no
# equivalent of macOS's per-insertion permission reset to work around.
if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: this script must be run as root (sudo) to write to /dev/$DISK"
    exit 1
fi

# Make sure nothing has auto-mounted the card's partitions (GNOME/KDE
# file managers do this automatically on insertion) before we write to
# them with debugfs/mke2fs.
for part in "$PART_ROOTFS" "$PART_DATA"; do
    if mountpoint_dir=$(lsblk -no MOUNTPOINT "$part" 2>/dev/null) && [ -n "$mountpoint_dir" ]; then
        echo "Unmounting $part (was mounted at $mountpoint_dir)..."
        umount "$part"
    fi
done

# --- Step 1: Patch rootfs (partition 1) ---
echo ""
echo "[1/3] Patching rootfs — adding OpenQiara autostart to rcS.real..."

# Dump the current rcS.real from the rootfs
TMPDIR=$(mktemp -d)
trap "rm -rf $TMPDIR" EXIT

$DEBUGFS -R "cat /etc/init.d/rcS.real" "$PART_ROOTFS" > "$TMPDIR/rcS.real" 2>/dev/null

# Check if already patched
if grep -q "OpenQiara autostart" "$TMPDIR/rcS.real"; then
    echo "  rcS.real already patched — skipping"
else
    # Append OpenQiara autostart
    # WiFi: hlconnman reads /data/wifi_ssid + /data/wifi_pass automatically
    cat >> "$TMPDIR/rcS.real" << 'PATCH'
# === OpenQiara autostart (added by sd_setup) ===
[ -x /data/boot.sh ] && /data/boot.sh >> /data/boot_debug.log 2>&1 &
PATCH
    printf "rm /etc/init.d/rcS.real\nwrite $TMPDIR/rcS.real /etc/init.d/rcS.real\nset_inode_field /etc/init.d/rcS.real mode 0100755\n" | $DEBUGFS -w "$PART_ROOTFS" 2>/dev/null
    echo "  rcS.real patched ✓"
fi

# --- Step 2: Write files to /data (partition 2) ---
echo ""
echo "[2/3] Formatting and writing files to /data partition..."

# Reformat with optimal settings: 4K blocks (efficient for the 10MB daemon
# binary), minimal inodes, no journal, no reserved blocks.
$MKE2FS -F -t ext4 -O ^has_journal -b 4096 -N 128 -m 0 "$PART_DATA" 2>/dev/null
echo "  partition formatted ✓"

# Write wifi credentials — printf '%s' preserves $ " \ space accents etc.
# Plain `echo` would interpret backslash escapes on some shells; redirection
# with >"$file" guarantees no trailing newline.
printf '%s' "$WIFI_SSID" > "$TMPDIR/wifi_ssid"
printf '%s' "$WIFI_PASS" > "$TMPDIR/wifi_pass"

# Write bridge marker
touch "$TMPDIR/bridge"

# Write boot.sh from scripts/camera_boot.sh (single source of truth).
write_boot_sh "$TMPDIR/boot.sh"
chmod +x "$TMPDIR/boot.sh"

# Build debugfs commands for data partition (freshly formatted, no rm needed)
DEBUGFS_CMDS=""
DEBUGFS_CMDS+="write $DAEMON_BIN openqiarad\n"
DEBUGFS_CMDS+="set_inode_field openqiarad mode 0100755\n"
DEBUGFS_CMDS+="write $TMPDIR/boot.sh boot.sh\n"
DEBUGFS_CMDS+="set_inode_field boot.sh mode 0100755\n"
DEBUGFS_CMDS+="write $TMPDIR/wifi_ssid wifi_ssid\n"
DEBUGFS_CMDS+="write $TMPDIR/wifi_pass wifi_pass\n"
DEBUGFS_CMDS+="write $TMPDIR/bridge bridge\n"

# SSH public key
if [ -n "$SSH_PUBKEY" ] && [ -f "$SSH_PUBKEY" ]; then
    DEBUGFS_CMDS+="write $SSH_PUBKEY ssh_authorized_keys\n"
fi

echo -e "$DEBUGFS_CMDS" | $DEBUGFS -w "$PART_DATA" 2>/dev/null
echo "  openqiarad ✓"
echo "  boot.sh ✓"
echo "  wifi_ssid ✓"
echo "  wifi_pass ✓"
echo "  bridge ✓"
[ -n "$SSH_PUBKEY" ] && echo "  ssh_authorized_keys ✓"

# --- Step 3: Verify ---
echo ""
echo "[3/3] Verifying..."

# Verify files exist on data partition
VERIFY=$($DEBUGFS -R "ls -l" "$PART_DATA" 2>/dev/null)
for f in openqiarad boot.sh wifi_ssid wifi_pass bridge; do
    if echo "$VERIFY" | grep -q "$f"; then
        echo "  /data/$f ✓"
    else
        echo "  /data/$f ✗ MISSING"
    fi
done

echo ""
echo "=== Done! ==="
echo ""
echo "Next steps:"
echo "  1. Eject the SD card: sudo eject /dev/$DISK"
echo "  2. Insert the SD card in the camera"
echo "  3. Power on the camera"
echo "  4. Wait ~60s for boot + WiFi connection"
echo "  5. Find the camera: arp -a | grep lwip   (or: ip neigh)"
echo "  6. Open http://<camera-ip>:80"
