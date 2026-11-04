#!/usr/bin/env bash
# Cross-compile openqiarad for ARM and deploy to a camera over SSH.
#
# Usage:
#   CAM_HOST=192.168.1.50 ./scripts/deploy.sh
#   CAM_HOST=192.168.1.50 SSH_KEY=~/.ssh/id_ed25519 ARGS='-web :80' ./scripts/deploy.sh

set -euo pipefail

CAM_HOST="${CAM_HOST:?CAM_HOST required (camera IP)}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
ARGS="${ARGS:--web :80}"
REMOTE_BIN="/data/openqiarad"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

SSH_CMD="ssh -i $SSH_KEY root@$CAM_HOST"

echo "==> Cross-compiling for ARM..."
GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o bin/openqiarad ./cmd/openqiarad
echo "    Build OK ($(du -h bin/openqiarad | cut -f1))"

echo "==> Uploading binary..."
# The binary lives on /media (large partition); /data is tiny (~20 MB) and
# only holds a symlink. Copying the binary into /data fills the disk and
# leaves a truncated, segfaulting binary — so we stage on /media and verify
# the byte count before swapping it in.
WANT=$(wc -c < bin/openqiarad)
gzip -c bin/openqiarad | $SSH_CMD 'gunzip > /media/openqiarad.new && chmod +x /media/openqiarad.new'
GOT=$($SSH_CMD 'wc -c < /media/openqiarad.new')
if [ "$GOT" != "$WANT" ]; then
	echo "!! Upload truncated: got $GOT bytes, expected $WANT (disk full on /media?)" >&2
	exit 1
fi

echo "==> Swapping the binary and stopping the old process..."
# boot.sh restarts openqiarad within ~10 s of its exit; a camera on an
# older boot.sh does not, so it is started here if nothing came back.
# A manual deploy ends any OTA trial: boot.sh would roll it back otherwise.
$SSH_CMD 'fbxupstartctl stop fbxhome 2>/dev/null; killall fbxhome 2>/dev/null
	mv -f /media/openqiarad.new /media/openqiarad && ln -sf /media/openqiarad /data/openqiarad
	rm -f /data/ota_trial
	killall openqiarad 2>/dev/null; true'
sleep 15
if ! $SSH_CMD 'pidof openqiarad >/dev/null'; then
	echo "==> Starting openqiarad ($ARGS)..."
	$SSH_CMD "nohup ${REMOTE_BIN} ${ARGS} > /data/openqiarad.log 2>&1 &"
fi

echo "==> Waiting 4s then tailing log..."
sleep 4
$SSH_CMD 'tail -50 /data/openqiarad.log'
