#!/bin/bash
# Install boot.sh on the camera and hook it into rcS.real

set -euo pipefail

CAM_HOST="${CAM_HOST:?CAM_HOST required (camera IP)}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
SSH_CMD="ssh -i ${SSH_KEY} root@${CAM_HOST}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "==> Copying boot.sh to camera ${CAM_HOST}..."
cat "${SCRIPT_DIR}/camera_boot.sh" | ${SSH_CMD} 'cat > /data/boot.sh'

echo "==> Making boot.sh executable..."
${SSH_CMD} "chmod +x /data/boot.sh"

echo "==> Hooking into rcS.real..."
${SSH_CMD} "
  if ! grep -q '/data/boot.sh' /etc/init.d/rcS.real; then
    mount -o remount,rw /
    cat >> /etc/init.d/rcS.real <<'BOOTEOF'

# === OpenQiara autostart ===
[ -x /data/boot.sh ] && /data/boot.sh &
BOOTEOF
    mount -o remount,ro /
    echo 'rcS.real patched.'
  else
    echo 'rcS.real already patched, skipping.'
  fi
"

echo "==> Done."
