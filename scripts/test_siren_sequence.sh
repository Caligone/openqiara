#!/usr/bin/env bash
# Envoie un payload hex arbitraire vers la SRN pour tester des opcodes.
#
# Usage:
#   ./scripts/test_siren_sequence.sh <payload_hex> [addr]
#
# Presets :
#   ./scripts/test_siren_sequence.sh beep     # bip pré-armement (subtype 02)
#   ./scripts/test_siren_sequence.sh disarm   # bip désarmement (subtype 03)
#
# Variables:
#   CAM_HOST (required, e.g. 192.168.1.50)
#   WEB_AUTH (required, e.g. admin:yourpassword)
#   NO_HANDSHAKE=1  → n'envoie pas 55 0b avant
#   NO_STOP=1       → n'envoie pas 55 05 00 84 après
#   HOLD_MS=3400    → attente entre payload et stop

set -euo pipefail

CAM_HOST="${CAM_HOST:?CAM_HOST required (e.g. 192.168.1.50)}"
WEB_AUTH="${WEB_AUTH:?WEB_AUTH required (e.g. admin:yourpassword)}"
ARG="${1:?Usage: $0 <hex_payload|preset> [addr]}"
ADDR="${2:-18}"

case "$ARG" in
  beep)   PAYLOAD="0155041e1e9605640200000000000000030000000000000003" ;;
  disarm) PAYLOAD="0155041e1e9605640300000000000000030000000000000003" ;;
  *)      PAYLOAD="$ARG" ;;
esac
PAYLOAD="${PAYLOAD// /}"

HANDSHAKE=true
[ "${NO_HANDSHAKE:-}" = "1" ] && HANDSHAKE=false
STOP=true
[ "${NO_STOP:-}" = "1" ] && STOP=false
HOLD_MS="${HOLD_MS:-3400}"

JSON=$(cat <<EOF
{"addr":$ADDR,"payload":"$PAYLOAD","handshake":$HANDSHAKE,"stop":$STOP,"hold_ms":$HOLD_MS}
EOF
)

echo "→ POST /api/debug/siren/sequence"
echo "   $JSON"
curl -sf -u "$WEB_AUTH" -X POST "http://$CAM_HOST:8080/api/debug/siren/sequence" \
  -H "Content-Type: application/json" -d "$JSON"
echo
