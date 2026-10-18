#!/usr/bin/env bash
# Push the Telegram config (made by telegram-setup) to the camera and restart
# openqiarad so it picks it up.
#
# Usage:
#   CAM_HOST=192.168.1.50 ./scripts/telegram_install.sh openqiara_telegram.json
#   CAM_HOST=192.168.1.50 SSH_KEY=~/.ssh/id_ed25519 ARGS='-web :80' ./scripts/telegram_install.sh openqiara_telegram.json

set -euo pipefail

CAM_HOST="${CAM_HOST:?CAM_HOST required (camera IP)}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
ARGS="${ARGS:--web :80}"
REMOTE_BIN="/data/openqiarad"
REMOTE_CFG="/data/openqiara_telegram.json"

CFG="${1:-}"
if [ -z "$CFG" ]; then
	echo "Usage : CAM_HOST=<ip> $0 <fichier openqiara_telegram.json>" >&2
	exit 1
fi
if [ ! -f "$CFG" ]; then
	echo "Fichier introuvable : $CFG (créez-le d'abord avec telegram-setup)" >&2
	exit 1
fi

SSH_CMD="ssh -i $SSH_KEY root@$CAM_HOST"

echo "==> Envoi de la configuration Telegram..."
# Written to a temporary file (mode 600 from creation), size-checked, then
# moved over the live file so a dropped connection never truncates it.
WANT=$(wc -c < "$CFG")
$SSH_CMD "umask 077; cat > $REMOTE_CFG.new && chmod 600 $REMOTE_CFG.new" < "$CFG"
GOT=$($SSH_CMD "wc -c < $REMOTE_CFG.new")
if [ "$GOT" != "$WANT" ]; then
	echo "Envoi incomplet : $GOT octets reçus sur $WANT (disque plein ?)" >&2
	exit 1
fi
$SSH_CMD "mv -f $REMOTE_CFG.new $REMOTE_CFG"

echo "==> Redémarrage d'openqiarad..."
# boot.sh restarts openqiarad within ~10 s of its exit; a camera on an
# older boot.sh does not, so it is started here if nothing came back.
$SSH_CMD 'killall openqiarad 2>/dev/null; true'
sleep 15
if ! $SSH_CMD 'pidof openqiarad >/dev/null'; then
	echo "==> Démarrage d'openqiarad ($ARGS)..."
	$SSH_CMD "nohup ${REMOTE_BIN} ${ARGS} -log /data/openqiarad.log > /dev/null 2>&1 &"
fi

echo "==> Terminé. Dernières lignes du journal :"
sleep 4
$SSH_CMD 'tail -20 /data/openqiarad.log'
