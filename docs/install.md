# Après l'installation

L'installation est dans le [README](../README.md#installation). Cette page couvre la maintenance d'une caméra déjà sous OpenQiara.

## Personnaliser le boot (`post_boot.sh`)

Si `/data/post_boot.sh` existe, `boot.sh` l'exécute à la fin du démarrage (sortie dans `/data/post_boot.log`). Les mises à jour remplacent `boot.sh` mais ne touchent jamais ce fichier : c'est l'endroit pour tes réglages locaux.

Exemple : n'autoriser en TCP que Home Assistant et le poste d'admin (proposé par @Avasse, #44).

```sh
# /data/post_boot.sh
iptables -D INPUT -p tcp -j ACCEPT              # retire l'ouverture TCP de boot.sh
iptables -I INPUT 3 -p tcp --dport 22 -j ACCEPT # SSH toujours ouvert, anti-lockout
for src in 192.168.1.10 192.168.1.20; do
    iptables -I INPUT 3 -s "$src" -j ACCEPT
done
```

> ⚠️ À utiliser en connaissance de cause :
> - **IPv6 n'est pas filtré** (le kernel n'a pas `ip6tables`) : l'allowlist se contourne par l'adresse IPv6 de la caméra.
> - **HomeKit casse** (port 51827) sauf si tu ajoutes tes appareils Apple, dont l'IP change avec le DHCP.
> - L'UDP reste ouvert : le bloquer casse le renouvellement DHCP et le flux RTP.
>
> Teste depuis une machine hors allowlist avant de t'y fier.

## Changer de réseau WiFi

```bash
# Via SSH
echo -n "NouveauSSID" > /data/wifi_ssid
echo -n "NouveauPass" > /data/wifi_pass
reboot
```

## En cas de perte d'accès

1. Créer un hotspot WiFi avec les credentials connus
2. Si rootfs corrompu : restaurer le backup via `dd` sur la SD depuis le Mac, Linux ou WSL (lecteur attaché avec usbipd, voir le [README](../README.md#2-sauvegarder-la-carte-sd)), ou repartir de zéro avec [`scripts/sd_setup.sh`](../scripts/sd_setup.sh) (installation hors-ligne via debugfs)
3. Si MCU corrompu : accès UART nécessaire (ESP32 bridge ou dongle CH340 3.3V)
