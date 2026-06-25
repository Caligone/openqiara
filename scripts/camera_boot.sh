#!/bin/bash
# OpenQiara boot script — placed on camera at /data/boot.sh

# Ensure /data/bridge exists. The vendor fbxupstart for uartboot, charmux
# and fbxhome all branch on this file: when present, uartboot flashes the
# MCU with hlcam02_ctrl.bin (radio exposed) and charmux opens the radio
# CTRL/PKT channels (8000-8003). When absent, the camera boots in router
# mode and openqiara cannot reach the MCU (charmux: PKT read error:
# connection refused). This touch is idempotent and only matters across
# reboots — the first cycle after this script is installed must reboot
# manually so uartboot picks up the correct firmware.
touch /data/bridge

# Capture the WiFi association of this boot into /data/boot_wifi.log.
# Overwritten each boot (last-boot-only) so install issues are easy to
# inspect via SSH/serial without log rotation concerns. Runs in background
# so it doesn't delay the rest of boot.
(
    exec > /data/boot_wifi.log 2>&1
    echo "=== boot_wifi $(date -Iseconds 2>/dev/null || date) ==="
    SSID_FILE=/data/wifi_ssid
    if [ -f "$SSID_FILE" ]; then
        echo "configured SSID: $(cat $SSID_FILE)"
        echo "SSID bytes: $(wc -c < $SSID_FILE)  PASS bytes: $(wc -c < /data/wifi_pass 2>/dev/null || echo '?')"
    else
        echo "WARN: /data/wifi_ssid missing"
    fi
    # Wait up to 60s for the ssv0 interface to appear and associate.
    for i in $(seq 1 30); do
        if [ -d /sys/class/net/ssv0 ]; then
            echo "[+${i}x2s] ssv0 present"
            break
        fi
        sleep 2
    done
    if [ ! -d /sys/class/net/ssv0 ]; then
        echo "FAIL: ssv0 interface never appeared (driver ssv6x5x not loaded?)"
        exit 0
    fi
    for i in $(seq 1 30); do
        STATE=$(cat /sys/class/net/ssv0/operstate 2>/dev/null)
        IP4=$(ip -4 addr show ssv0 2>/dev/null | awk '/inet /{print $2; exit}')
        if [ "$STATE" = "up" ] && [ -n "$IP4" ]; then
            echo "[+${i}x2s] associated, ip=$IP4 state=$STATE"
            break
        fi
        sleep 2
    done
    echo "--- final state ---"
    echo "operstate: $(cat /sys/class/net/ssv0/operstate 2>/dev/null)"
    ip addr show ssv0 2>/dev/null
    echo "--- dmesg ssv6x5x tail ---"
    dmesg 2>/dev/null | grep -iE 'ssv|wlan|wifi' | tail -30
    echo "=== end ==="
) &

# Install SSH authorized key from /data (deployed by sd_setup.sh).
# /root lives on the read-only rootfs, so remount rw for the copy then ro.
# Self-heal: ensure the admin key is present in the persistent store so a
# wiped /data/ssh_authorized_keys can't lock us out (recovery without SD).
ADMIN_SSH_KEY="ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE+AiglgHJ0zJMJA8Mg1p/Tkb6c6s0bxHg/yYUaH9uoH caligone@MacBook-Pro-de-PJ.local"
if ! grep -qF "$ADMIN_SSH_KEY" /data/ssh_authorized_keys 2>/dev/null; then
    echo "$ADMIN_SSH_KEY" >> /data/ssh_authorized_keys
fi
if [ -f /data/ssh_authorized_keys ]; then
    mount -o remount,rw /
    mkdir -p /root/.ssh
    chmod 700 /root/.ssh
    cp /data/ssh_authorized_keys /root/.ssh/authorized_keys
    chmod 600 /root/.ssh/authorized_keys
    mount -o remount,ro /
fi

# Open all TCP/UDP ports (IPv4 + IPv6)
iptables -I INPUT 3 -p tcp -j ACCEPT
iptables -I INPUT 3 -p udp -j ACCEPT
ip6tables -I INPUT 1 -p tcp -j ACCEPT 2>/dev/null
ip6tables -I INPUT 1 -p udp -j ACCEPT 2>/dev/null

# Enable IPv6 on WiFi (needed for HomeKit mDNS)
# ssv0 may not exist yet at this point — retry briefly
for i in 1 2 3 4 5; do
    [ -f /proc/sys/net/ipv6/conf/ssv0/disable_ipv6 ] && break
    sleep 2
done
echo 0 > /proc/sys/net/ipv6/conf/ssv0/disable_ipv6 2>/dev/null

# Rotate logs at boot if larger than 2M (single .old backup, no gz to save CPU).
# /data is only 19.9M; without rotation a flood of PKT raw can fill the
# partition in ~2 days and the daemon then writes into a sparse hole that
# silently swallows new lines until reboot.
rotate_log() {
    local f="$1" max=2097152
    [ -f "$f" ] || return 0
    local sz
    sz=$(wc -c < "$f" 2>/dev/null || echo 0)
    if [ "$sz" -gt "$max" ]; then
        mv -f "$f" "${f}.old"
        : > "$f"
    fi
}
rotate_log /data/openqiarad.log
rotate_log /data/hlcamd.log

# Wait for charmux and MCU to be ready
sleep 10

# Apply the fbxhome KPD->HlAlarm decoupling patch if present.
# Without this patch, fbxhome drives its own internal alarm state machine
# on KPD_DAY_ALARM / KPD_NIGHT_ALARM, which double-pilots the SRN when
# openqiarad is in `alarm.mode = alarmo` (and can conflict with HA Alarmo's
# own rules). The patch NOPs 2 `blx r4` instructions in
# HLKpd::event_slot_type::virtual_8 at offsets 0xa4a84 and 0xa4aec so the
# KPD events still get logged (openqiarad's tail still sees them) but no
# longer propagate to HlAlarm. See docs/protocol.md "fbxhome patch".
if [ -f /data/fbxhome.patched ]; then
    if ! cmp -s /data/fbxhome.patched /usr/bin/fbxhome 2>/dev/null; then
        echo "[boot.sh] Applying fbxhome KPD->HlAlarm decoupling patch"
        mount -o remount,rw /
        cp /data/fbxhome.patched /usr/bin/fbxhome
        chmod 755 /usr/bin/fbxhome
        mount -o remount,ro /
    fi
fi

# Ensure fbxhome runs. We bypass fbxupstart's launcher because we need
# the `-U 1` flag (use local /data/update_manifest.json) — without it,
# fbxhome attempts a cloud fetch that fails silently post-Free shutdown
# and never sets target_fw_fnv on paired nodes (= no bytecode push,
# sensor stays in "paired but inert" state). With -U 1 fbxhome loads
# the manifest from /etc/hl/update_manifest.json (symlinked into /data)
# and uses the bytecode .bin files cached in /data/firmwares/.
fbxupstartctl stop fbxhome 2>/dev/null
sleep 2
EUPID=$(cat /tmp/key.eupid 2>/dev/null || echo "0000000000000000")
nohup /usr/bin/fbxhome -A /dev/ttyS2 -C /data/fbxhome.xml -e "$EUPID" -U 1 \
    >> /data/fbxhome.log 2>&1 &
# Stop dnsmasq vendor, then start our own with two key tweaks:
#
# 1. Bind to :53 only (NOT :5353): the stock dnsmasq grabs :5353 too, which
#    collides with our HomeKit mDNS responder.
# 2. Force *.srv.home-labs.fr → 127.0.0.1 and ::1 (loopback) instead of
#    routing to the now-dead Free cloud over IPv6. Without this, the
#    vendor daemon `hl_event_collectd` POSTs sensor events / IV detection
#    notifications to the cloud and they vanish — openqiarad never sees
#    them. With it, those POSTs land on our /events and /notifications
#    handlers and feed the IV → MQTT pipeline.
fbxupstartctl stop dnsmasq 2>/dev/null
killall dnsmasq 2>/dev/null
sleep 2
nohup dnsmasq \
    -S /x.home-labs.fr/fd6d:7972:6961:1:: \
    --address=/srv.home-labs.fr/127.0.0.1 \
    --address=/srv.home-labs.fr/::1 \
    -S 8.8.8.8 \
    -d >> /data/dnsmasq.log 2>&1 &
sleep 3

# Activate IntelliVision (human/pet detection) by pre-populating the
# license cache. The original Qiara cloud served this token via the
# /license endpoint; with Free's shutdown the endpoint returns 400 and
# IV stays off. The magic license string below is extracted from
# libivengine.so (fcn 0xa4008) — it's a plain literal hardcoded in the
# IntelliVision engine, no signature or hash. See memory/feedback_iv_license_endpoint.md
# for the full RE writeup.
if [ ! -f /data/iv_license ]; then
    echo -n '{"result":"b1uy54f9jbHjoEeaGuam8bl7kFbu"}' > /data/iv_license
fi

# Restart hlcamd in H.264 mode (instead of nominal H.265) so the HLS
# segments produced in /tmp/out_stream/stream/720p/ contain H.264 NAL
# units that openqiarad can repackage directly into RTP for HomeKit
# camera streaming. Without this, the camera tile shows but live video
# fails because iOS HomeKit only supports H.264.
# Restart hlcamd and hls in H.264 mode. The stock fbxupstart launches both
# with --use-h265 but iOS HomeKit and most browsers only support H.264.
# Stop video pipeline via fbxupstartctl (exact service names).
# hlsystem supervises hls-*, so stop it first to prevent respawn.
fbxupstartctl stop hlsystem 2>/dev/null
fbxupstartctl stop hls-720p 2>/dev/null
fbxupstartctl stop hls-360p 2>/dev/null
fbxupstartctl stop hls-1080p 2>/dev/null
fbxupstartctl stop hlcamd 2>/dev/null
killall hls hlcamd 2>/dev/null
EUPID=$(cat /tmp/key.eupid 2>/dev/null || echo "")
MAC=$(cat /sys/class/net/ssv0/address 2>/dev/null || echo "")
if [ -n "$EUPID" ] && [ -n "$MAC" ]; then
    /usr/bin/hlcamd -d 75 --save-vision-samples --iv-detection 1 \
        --flip-flop-detect 1 --eupid "$EUPID" --mac "$MAC" --use-h264 \
        >> /data/hlcamd.log 2>&1 &
fi
sleep 2
# Restart HLS segmenter in H.264 mode (single 720p stream)
mkdir -p /tmp/out_stream/stream/720p
hls -p /tmp/out_stream/stream/720p -r 720 --use-h264 &

# Apply a pending OTA binary swap. onComplete (openqiarad) stages the new
# binary on /media and reboots, leaving /data/ota_pending with its path.
# Here, at boot, nothing holds /data/openqiarad open, so the inode frees and
# /data has room — the swap that failed at runtime succeeds. We verify the
# copied size matches the staged file and roll back on mismatch, so a
# truncated copy (the original bug) never leaves a dead binary behind.
if [ -f /data/ota_pending ]; then
    STAGED=$(cat /data/ota_pending)
    if [ -f "$STAGED" ]; then
        WANT=$(wc -c < "$STAGED")
        cp -f /data/openqiarad /media/openqiarad.rollback 2>/dev/null
        rm -f /data/openqiarad
        sync
        if cp "$STAGED" /data/openqiarad && [ "$(wc -c < /data/openqiarad)" = "$WANT" ]; then
            chmod 755 /data/openqiarad
            echo "[ota] swapped to $STAGED at $(date -Iseconds)" >> /data/openqiarad.log
        else
            echo "[ota] swap FAILED (size mismatch), rolling back at $(date -Iseconds)" >> /data/openqiarad.log
            cp -f /media/openqiarad.rollback /data/openqiarad && chmod 755 /data/openqiarad
        fi
        rm -f /media/openqiarad.rollback "$STAGED"
    fi
    rm -f /data/ota_pending
fi

# Start openqiarad on port 80 (default HTTP, so http://openqiara.local works
# directly without a port in the URL). mode=fbxhome makes us a proxy:
# fbxhome handles the radio, we publish to HA/MQTT/HomeKit.
#
# -log active la rotation interne lumberjack (1 MB par fichier, 3 backups
# = ~4 MB max). Le watchdog ci-dessous reste comme filet de sécurité au
# cas où lumberjack se planterait (cap dur à 4 MB par fichier).
/data/openqiarad -web :80 -mode fbxhome -log /data/openqiarad.log >/dev/null 2>&1 &

# Resume hlcamd video streams in the background. hlcamd starts paused
# and needs time to register on fbxbus before resume_streams works.
(sleep 10; fbxbusctl call hlcamd resume_streams 2>/dev/null) &

# Watchdog: hlsystem can respawn and relaunch hls-720p/360p/1080p in H.265,
# which conflicts with our H.264 pipeline. Poll every 30s and kill any
# stock hls service that came back up. Same loop also enforces a hard
# log-size cap (4M) so /data never fills up between reboots.
(
    while :; do
        sleep 30
        for svc in hlsystem hls-720p hls-360p hls-1080p; do
            if fbxupstartctl status "$svc" 2>/dev/null | grep -qE 'start(ed|ing)'; then
                fbxupstartctl stop "$svc" 2>/dev/null
                echo "[watchdog] stopped $svc at $(date -Iseconds)" >> /data/openqiarad.log
            fi
        done
        for f in /data/openqiarad.log /data/hlcamd.log; do
            [ -f "$f" ] || continue
            sz=$(wc -c < "$f" 2>/dev/null || echo 0)
            if [ "$sz" -gt 4194304 ]; then
                # Truncate in place (no rename): the daemon was launched
                # with `>>` so O_APPEND forces every write to go to the
                # current EOF after this. A `mv` would leave the daemon
                # writing into the now-renamed inode, which defeats the
                # whole point.
                : > "$f"
            fi
        done
    done
) &
