#!/usr/bin/env bash
# Prepare a Qiara camera SD card for OpenQiara — entirely offline from a Mac.
#
# Prerequisites:
#   - macOS with e2fsprogs installed (brew install e2fsprogs)
#   - The SD card from a stock Qiara camera inserted in the Mac
#   - A pre-built openqiarad binary (run: make build-arm)
#
# Usage:
#   ./scripts/sd_setup.sh --disk disk4 --wifi-ssid "MyNetwork" --wifi-pass "MyPassword"
#
# Options:
#   --disk         macOS disk identifier (e.g. disk4) — REQUIRED
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

# e2fsprogs tools (Homebrew puts them in sbin, not in PATH)
DEBUGFS="$(brew --prefix e2fsprogs 2>/dev/null)/sbin/debugfs"
if [ ! -x "$DEBUGFS" ]; then
    echo "ERROR: debugfs not found. Install with: brew install e2fsprogs"
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
    echo "Usage: $0 --disk <diskN> --wifi-ssid <ssid> --wifi-pass <password>"
    echo ""
    echo "Find your SD card disk with: diskutil list"
    exit 1
fi

PART_ROOTFS="/dev/${DISK}s1"
PART_DATA="/dev/${DISK}s2"

# Sanity checks
if [ ! -b "/dev/${DISK}" ]; then
    echo "ERROR: /dev/${DISK} does not exist"
    exit 1
fi

# Verify this looks like a Qiara SD card (3 Linux partitions)
PART_COUNT=$(diskutil list "$DISK" | grep -c "Linux" || true)
if [ "$PART_COUNT" -lt 3 ]; then
    echo "ERROR: Expected 3 Linux partitions on $DISK, found $PART_COUNT"
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

# macOS resets device permissions on each SD insert — fix them
echo "Fixing device permissions (requires sudo)..."
sudo chmod 666 /dev/${DISK}s*

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
MKE2FS="$(brew --prefix e2fsprogs 2>/dev/null)/sbin/mke2fs"
$MKE2FS -F -t ext4 -O ^has_journal -b 4096 -N 128 -m 0 "$PART_DATA" 2>/dev/null
echo "  partition formatted ✓"

# Write wifi credentials
echo -n "$WIFI_SSID" > "$TMPDIR/wifi_ssid"
echo -n "$WIFI_PASS" > "$TMPDIR/wifi_pass"

# Write bridge marker
touch "$TMPDIR/bridge"

# Copy boot.sh
cp "$REPO_ROOT/scripts/camera_boot.sh" "$TMPDIR/boot.sh"
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
echo "  1. Eject the SD card:  diskutil eject $DISK"
echo "  2. Insert the SD card in the camera"
echo "  3. Power on the camera"
echo "  4. Wait ~60s for boot + WiFi connection"
echo "  5. Find the camera:  arp -a | grep lwip"
echo "  6. Open http://<camera-ip>:80"
