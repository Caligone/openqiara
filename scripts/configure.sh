#!/bin/bash
# Write openqiara.json config to the camera via SSH.
#
# Usage:
#   CAM_HOST=192.168.1.50 SSH_KEY=~/.ssh/id_ed25519 \
#   MQTT_BROKER=tcp://192.168.1.10:1883 \
#   MQTT_USER=openqiara MQTT_PASS=changeme \
#   HOMEKIT_PIN=00102003 \
#   ./scripts/configure.sh

set -euo pipefail

CAM_HOST="${CAM_HOST:?CAM_HOST required (camera IP)}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
MQTT_BROKER="${MQTT_BROKER:?MQTT_BROKER required (e.g. tcp://192.168.1.10:1883)}"
MQTT_USER="${MQTT_USER:-openqiara}"
MQTT_PASS="${MQTT_PASS:-}"
HOMEKIT_PIN="${HOMEKIT_PIN:-00102003}"

SSH_CMD="ssh -i ${SSH_KEY} root@${CAM_HOST}"

echo "==> Writing openqiara.json to camera ${CAM_HOST}..."

${SSH_CMD} 'cat > /data/openqiara.json' <<CONFIGEOF
{
  "mqtt": {
    "broker": "${MQTT_BROKER}",
    "username": "${MQTT_USER}",
    "password": "${MQTT_PASS}",
    "topic_prefix": "openqiara"
  },
  "homekit": {
    "enabled": true,
    "pin": "${HOMEKIT_PIN}",
    "name": "OpenQiara"
  },
  "admin": {}
}
CONFIGEOF

echo "==> Done. Config written."
