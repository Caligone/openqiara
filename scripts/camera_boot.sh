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

# Install the user's SSH authorized key from /data into /root/.ssh.
# The key comes solely from /data/ssh_authorized_keys, which the user
# provisions at flash time via `sd_setup.sh --ssh-pubkey <their key>`.
# /root lives on the read-only rootfs, so remount rw for the copy then ro.
# No key is ever embedded in this script: it must not authorize anyone
# but the operator who flashed the camera. If /data/ssh_authorized_keys
# is wiped, re-provision it from the SD (sd_setup.sh) — losing it must
# lock the camera down, not fall back to a built-in key.
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
# dnsmasq/boot_debug pile up on the tiny /data partition (20 MB) — cap them
# too, or the SD fills and the camera fails to boot (reported on the forum).
rotate_log /data/dnsmasq.log
rotate_log /data/boot_debug.log

# Purge lumberjack's timestamped rotations (openqiarad-2026-...-.log) and any
# orphaned OTA binaries left on /media by past installs or manual deploys.
# Keep only: the live binary (/media/openqiarad, no suffix), its .old rollback,
# the .new pending-swap, and the .rollback. Everything else (.bak*, .pre*,
# _new, .rc2upx…) is a leftover safe to remove.
rm -f /data/openqiarad-*.log 2>/dev/null
for b in /media/openqiarad /media/openqiarad.* /media/openqiarad_*; do
    [ -e "$b" ] || continue
    case "$b" in
        /media/openqiarad|/media/openqiarad.old|/media/openqiarad.new|/media/openqiarad.rollback) ;;
        *) rm -f "$b" 2>/dev/null ;;
    esac
done

# Wait for charmux and MCU to be ready
sleep 10

# fbxhome, the vendor's radio daemon, must not run: openqiarad is the
# radio gateway and needs the charmux ports fbxhome would hold. fbxupstart
# may have started it already. Its state (/data/fbxhome.xml.*) stays:
# openqiarad imports the paired sensors from it at its first start.
fbxupstartctl stop fbxhome 2>/dev/null
killall fbxhome 2>/dev/null
rm -f /data/fbxhome.log /data/fbxhome.log.old
# Stop dnsmasq vendor, then start our own with two key tweaks:
#
# 1. Bind to :53 only (NOT :5353): the stock dnsmasq grabs :5353 too, which
#    collides with our HomeKit mDNS responder.
# 2. Force *.srv.home-labs.fr → 127.0.0.1 and ::1 (loopback) instead of
#    routing to the now-dead Free cloud over IPv6: what vendor code still
#    calls it (hlcamd's IntelliVision licence check, should
#    /data/iv_license go) stays on the camera.
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

# Restart hlcamd in H.264 mode: fbxupstart starts it in H.265, which iOS
# HomeKit and most browsers don't play. Stop the vendor's video services
# first (exact service names); hlsystem supervises hls-*, so it goes first
# to prevent respawn.
fbxupstartctl stop hlsystem 2>/dev/null
fbxupstartctl stop hls-720p 2>/dev/null
fbxupstartctl stop hls-360p 2>/dev/null
fbxupstartctl stop hls-1080p 2>/dev/null
fbxupstartctl stop hlcamd 2>/dev/null
killall hls hlcamd 2>/dev/null
# hlcamd started too soon after the previous one is killed dies at init
# (MI_VIF_SetDevAttr FAILED a0062082) and leaves the video input stuck
# until the next reboot. Wait until the old instances are really gone.
for i in $(seq 1 15); do
    pidof hlcamd >/dev/null || pidof hls >/dev/null || break
    sleep 1
done
sleep 2
EUPID=$(cat /tmp/key.eupid 2>/dev/null || echo "")
MAC=$(cat /sys/class/net/ssv0/address 2>/dev/null || echo "")
# openqiarad reads the main stream (1080p, multicast 9600) and the
# microphone (9700). The 720p and 360p encodes fed the vendor's hls only:
# off, about a quarter of a core and 14 MB saved (measured 2026-10-10),
# person detection unaffected.
if [ -n "$EUPID" ] && [ -n "$MAC" ]; then
    /usr/bin/hlcamd -d 75 --iv-detection 1 \
        --flip-flop-detect 1 --eupid "$EUPID" --mac "$MAC" --use-h264 --no-720p --no-360p \
        >> /data/hlcamd.log 2>&1 &
fi
sleep 2
# hls, the vendor's segmenter, stays off: openqiarad serves the HLS of
# the web view itself (internal/hlsserver), from what hlcamd multicasts.

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
# directly without a port in the URL). openqiarad is the radio gateway:
# it serves the sensors and publishes to HA/MQTT/HomeKit.
#
# -log active la rotation interne lumberjack (1 MB par fichier, 3 backups
# = ~4 MB max). Le watchdog ci-dessous reste comme filet de sécurité au
# cas où lumberjack se planterait (cap dur à 4 MB par fichier).
#
# Restarted when it exits: it is the radio gateway, nothing else serves the
# sensors. It exits when the radio is not free yet (fbxhome still letting
# go of it) or on a crash; to deploy, replace the binary and kill it.
(
    while :; do
        /data/openqiarad -web :80 -log /data/openqiarad.log >/dev/null 2>&1
        echo "[boot] openqiarad exited ($?) at $(date -Iseconds), restarting" >> /data/openqiarad.log
        sleep 10
    done
) &

# Stop the vendor watchdog from rebooting the camera every ~12 h 06.
# watchdog_mcu keeps the MCU's 15-minute hardware watchdog fed only while
# the Free cloud VPN exchanged keys within its timeout, 43200 s by default
# (RE 2026-10-02): the cloud is dead, so 12 h after boot it stops and the
# MCU powers the camera off. set_timeout raises that limit (~63 years);
# its other checks (hlcamd alive, services up) still apply. watchdog_mcu
# starts once myriadvpn is up, hence the retries.
(
    for i in $(seq 1 30); do
        fbxbusctl call watchdog_mcu set_timeout 2000000000 >/dev/null 2>&1 && break
        sleep 10
    done
) &

# hlcamd starts paused: openqiarad resumes its streams, or keeps them
# paused while the privacy shutter is closed.

# Watchdog: hlsystem can respawn and relaunch hls-720p/360p/1080p in H.265,
# which conflicts with our H.264 pipeline, and fbxhome would take the radio
# back. Poll every 30s and stop any stock service that came back up. Same loop also enforces a hard
# log-size cap (4M) so /data never fills up between reboots.
(
    while :; do
        sleep 30
        # The dead cloud's services too, myriadvpn only once openqiarad
        # feeds the MCU's watchdog (watchdog_mcu stopped): while
        # watchdog_mcu runs, myriadvpn down means a reboot.
        # nginx-pub fronted fbxhome's API for the app (all its upstreams are
        # gone) and cron restarts it every midnight: kept stopped too.
        # hl-event-collectd: openqiarad serves its name on fbxbus; alive,
        # it would keep hlcamd's direct (p2p) link.
        cloud="downloader srt-daemon nginx-pub hl-event-collectd"
        pidof watchdog_mcu >/dev/null || cloud="$cloud myriadvpn"
        for svc in hlsystem hls-720p hls-360p hls-1080p fbxhome $cloud; do
            if fbxupstartctl status "$svc" 2>/dev/null | grep -qE 'start(ed|ing)'; then
                fbxupstartctl stop "$svc" 2>/dev/null
                echo "[watchdog] stopped $svc at $(date -Iseconds)" >> /data/openqiarad.log
            fi
        done
        # openqiarad + hlcamd are launched with `>>` (O_APPEND), so a
        # truncate-in-place makes the next write land at the new EOF.
        # 2M cap each leaves headroom on the 16M /data for everything else.
        for f in /data/openqiarad.log /data/hlcamd.log; do
            [ -f "$f" ] || continue
            sz=$(wc -c < "$f" 2>/dev/null || echo 0)
            if [ "$sz" -gt 2097152 ]; then
                : > "$f"
            fi
        done
        # boot_debug.log (native SIGHUP flood) is NOT ours and its writer
        # keeps a raw fd on the inode: verified 2026-09-13 that after `mv` it
        # keeps appending to the renamed file at the old offset, and after
        # truncate it punches a sparse hole. Neither is safe at runtime, so
        # we leave it to the boot-time rotate_log pass (its writer gets a
        # fresh fd only at boot). We just kill the *.old left behind, dead
        # weight that alone can fill /data.
        rm -f /data/boot_debug.log.old /data/dnsmasq.log.old 2>/dev/null
    done
) &

# User hook, run last: local tweaks (firewall rules…) live in
# /data/post_boot.sh, which updates never touch, unlike this file.
# See docs/install.md § « Personnaliser le boot ».
[ -f /data/post_boot.sh ] && sh /data/post_boot.sh >> /data/post_boot.log 2>&1
