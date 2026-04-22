#!/bin/bash
# Install OpenQiara custom rootfs on a running Qiara camera via SSH
# Usage: ./install-rootfs.sh --host <CAMERA_IP> --ssh-key ~/.ssh/id_ed25519
set -e

HOST=""
SSH_KEY=""
WIFI_SSID=""
WIFI_PASS=""
DAEMON_BIN=""
ADMIN_PASS=""
SSH_PUBKEY=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --host) HOST="$2"; shift 2;;
        --ssh-key) SSH_KEY="-i $2"; shift 2;;
        --wifi-ssid) WIFI_SSID="$2"; shift 2;;
        --wifi-pass) WIFI_PASS="$2"; shift 2;;
        --daemon) DAEMON_BIN="$2"; shift 2;;
        --password) ADMIN_PASS="$2"; shift 2;;
        --ssh-pubkey) SSH_PUBKEY="$2"; shift 2;;
        *) echo "Unknown: $1"; exit 1;;
    esac
done

if [ -z "$HOST" ]; then
    echo "Usage: $0 --host <camera-ip> [--ssh-key <key>] [--wifi-ssid <ssid>] [--wifi-pass <pass>] [--daemon <path>] [--ssh-pubkey <path>]"
    exit 1
fi

SSH="ssh -o StrictHostKeyChecking=no $SSH_KEY root@$HOST"
SCP="scp -O -o StrictHostKeyChecking=no $SSH_KEY"

echo "=== OpenQiara Rootfs Install ==="
echo "Host: $HOST"

# Step 1: Verify camera is reachable and running Qiara
echo "[1/7] Checking camera..."
$SSH "test -f /usr/bin/charmux && echo OK" || { echo "Camera not reachable or not Qiara"; exit 1; }

# Step 2: Remount rootfs read-write
echo "[2/7] Remounting rootfs..."
$SSH "mount -o remount,rw /"

# Step 3: Clean stock binaries
echo "[3/7] Cleaning stock system..."
$SSH '
# Remove fbxhome and variants
rm -f /usr/bin/fbxhome /usr/bin/fbxhome.*
# Remove stock services we replace
rm -f /usr/bin/fbxupstart /usr/bin/fbxupstartctl
rm -f /usr/bin/hlconnmand
rm -f /usr/bin/hlbringup
rm -f /usr/bin/fbxntp
rm -f /usr/sbin/nginx
rm -f /usr/bin/myriadvpn
rm -f /usr/bin/fbxbusd /usr/bin/fbxbusctl
# Remove fbxupstart configs
rm -rf /etc/fbxupstart.d/
rm -f /etc/fbxupstart.conf
# Remove nginx config
rm -rf /etc/nginx/
echo "Cleaned"
'

# Step 4: Install custom init
echo "[4/7] Installing custom init..."
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
$SCP "$SCRIPT_DIR/../rootfs/init/rcS" root@$HOST:/tmp/rcS.new
$SSH '
cp /tmp/rcS.new /etc/init.d/rcS.real
chmod 755 /etc/init.d/rcS.real
echo "Init installed"
'

# Step 5: Install openqiarad
echo "[5/7] Installing openqiarad..."
if [ -n "$DAEMON_BIN" ] && [ -f "$DAEMON_BIN" ]; then
    gzip -c "$DAEMON_BIN" > /tmp/openqiarad.gz
    $SCP /tmp/openqiarad.gz root@$HOST:/data/openqiarad.gz
    $SSH 'rm -f /data/openqiarad; gunzip /data/openqiarad.gz; chmod +x /data/openqiarad; echo "Daemon installed: $(wc -c < /data/openqiarad) bytes"'
else
    echo "  (skipped — use --daemon to specify binary)"
fi

# Step 6: Configure WiFi
echo "[6/7] Configuring..."
if [ -n "$WIFI_SSID" ]; then
    $SSH "echo -n '$WIFI_SSID' > /data/wifi_ssid; echo -n '$WIFI_PASS' > /data/wifi_pass; echo 'WiFi: $WIFI_SSID'"
fi

if [ -n "$SSH_PUBKEY" ] && [ -f "$SSH_PUBKEY" ]; then
    $SCP "$SSH_PUBKEY" root@$HOST:/data/ssh_authorized_keys
    $SSH 'mkdir -p /root/.ssh; chmod 700 /root/.ssh; cp /data/ssh_authorized_keys /root/.ssh/authorized_keys; chmod 600 /root/.ssh/authorized_keys; echo "SSH key installed"'
fi

# Step 7: Verify
echo "[7/7] Verifying..."
$SSH '
echo "Init: $(ls -la /etc/init.d/rcS.real | awk "{print \$5}") bytes"
echo "Daemon: $(ls -la /data/openqiarad 2>/dev/null | awk "{print \$5}") bytes"
echo "WiFi: $(cat /data/wifi_ssid 2>/dev/null)"
echo "SSH key: $(wc -c < /root/.ssh/authorized_keys 2>/dev/null || echo "none") bytes"
echo "charmux: $(which charmux)"
echo "uartboot: $(which uartboot)"
echo "dropbear: $(which dropbear)"
'

echo ""
echo "=== Done! Reboot the camera to apply. ==="
echo "After reboot, access http://$HOST:8080"
