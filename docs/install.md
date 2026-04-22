# Installation — méthode SSH (alternative)

Cette procédure est une alternative à `scripts/sd_setup.sh` (méthode hors-ligne via debugfs). À utiliser si la caméra est déjà sur le réseau et accessible en SSH.

Pour la méthode offline recommandée, voir [`getting-started.md`](getting-started.md) et [`../scripts/sd_setup.sh`](../scripts/sd_setup.sh).

## Prérequis

- Caméra Qiara déjà provisionnée et accessible en SSH
- Mac/Linux avec Go 1.21+
- Clé SSH ed25519 (ex: `~/.ssh/id_ed25519`)
- Réseau WiFi 2.4GHz

## Procédure

### 1. Accès initial via SSH

La caméra stock se connecte au WiFi configuré lors du setup Qiara. Les credentials WiFi sont stockés par `hlconnman` en interne et dans `/dev/mmcblk0p3` (partition media) : fichiers `wifi_ssid` et `wifi_pass`.

Si la caméra est déjà sur le réseau :
```bash
ssh -i ~/.ssh/id_ed25519 root@<IP_CAMERA>
```

Pour trouver l'IP : `arp -a | grep lwip`

### 2. Patch du rootfs via SSH

Le rootfs stock (partition `mmcblk0p1`) est monté read-only au boot. Pour le modifier depuis SSH :

```bash
# Depuis SSH sur la caméra
mount -o remount,rw /

# Ajouter les services avant "step done" dans rcS.real
cat >> /tmp/rcS_additions << 'EOF'
# === iptables ===
iptables -I INPUT 3 -p tcp --dport 80 -j ACCEPT

# === wifi_force ===
(
sleep 20
SSID=$(cat /data/wifi_ssid 2>/dev/null)
PASS=$(cat /data/wifi_pass 2>/dev/null)
if [ -n "$SSID" ]; then
    fbxbusctl call hlconnman join "$SSID" "$PASS" 2>/dev/null
fi
) &

# === dropbear_keygen ===
mkdir -p /tmp/dropbear
dropbearkey -t ecdsa -f /tmp/dropbear/dropbear_ecdsa_host_key 2>/dev/null
dropbearkey -t ed25519 -f /tmp/dropbear/dropbear_ed25519_host_key 2>/dev/null
dropbearkey -t rsa -s 2048 -f /tmp/dropbear/dropbear_rsa_host_key 2>/dev/null

# === openqiara ===
(sleep 45; /data/openqiarad -web :80 -mode fbxhome >> /data/openqiarad.log 2>&1) &
EOF

sed -i "/^step \"done\"/r /tmp/rcS_additions" /etc/init.d/rcS.real

# Ajouter iptables après firewall.rules
sed -i 's|iptables-restore < /etc/firewall.rules|iptables-restore < /etc/firewall.rules\niptables -I INPUT 3 -p tcp --dport 80 -j ACCEPT|' /etc/init.d/rcS.real

rm /tmp/rcS_additions
mount -o remount,ro /
```

### 3. Déployer openqiarad

```bash
# Sur le Mac : cross-compiler
cd openqiara
GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" -o bin/openqiarad ./cmd/openqiarad
gzip -kf bin/openqiarad

# Uploader (pas de SCP — utiliser cat pipe)
cat bin/openqiarad.gz | ssh -i ~/.ssh/id_ed25519 root@<IP> 'gunzip > /data/openqiarad && chmod +x /data/openqiarad'
```

### 4. Configurer WiFi et MQTT

```bash
# Sur la caméra
echo -n "MonSSID" > /data/wifi_ssid
echo -n "MonMotDePasse" > /data/wifi_pass

cat > /data/openqiara.json << 'EOF'
{
  "mqtt": {
    "broker": "tcp://192.168.1.42:1883",
    "username": "openqiara",
    "password": "openqiara123",
    "topic_prefix": "openqiara"
  },
  "homekit": {
    "enabled": false,
    "pin": "00102003",
    "name": "OpenQiara"
  },
  "admin": {}
}
EOF
```

### 5. Reboot et vérifier

```bash
reboot
# Attendre ~60s
ssh -i ~/.ssh/id_ed25519 root@<IP>
# Vérifier
pidof openqiarad && echo "OK"
curl -s http://127.0.0.1/api/status
```

### 6. Faire les backups (OBLIGATOIRE)

```bash
# Sur la caméra — sauvegarder le rootfs patché
dd if=/dev/mmcblk0p1 | gzip > /data/rootfs_backup.gz
# Récupérer sur le Mac
ssh root@<IP> 'cat /data/rootfs_backup.gz' > ./rootfs_backup_patched.gz
```

## Changer de réseau WiFi

```bash
# Via SSH
echo -n "NouveauSSID" > /data/wifi_ssid
echo -n "NouveauPass" > /data/wifi_pass
reboot
```

## En cas de perte d'accès

1. Créer un hotspot WiFi avec les credentials connus
2. Si rootfs corrompu : restaurer le backup via `dd` sur la SD depuis le Mac, ou repartir de zéro avec [`scripts/sd_setup.sh`](../scripts/sd_setup.sh) (installation hors-ligne via debugfs)
3. Si MCU corrompu : accès UART nécessaire (ESP32 bridge ou dongle CH340 3.3V)
