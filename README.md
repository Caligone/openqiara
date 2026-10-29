# OpenQiara

Un remplaçant 100% local de la couche applicative cloud des caméras de sécurité
Qiara.

Qiara était une startup française d'alarme-caméra qui a coupé ses serveurs,
laissant ses clients avec des appareils briqués. OpenQiara les remet en service
en faisant tourner un petit daemon Go (`openqiarad`) directement sur la caméra,
qui dialogue avec les capteurs 868 MHz existants et les expose à
**Home Assistant** via MQTT.

```
┌──────────────┐      MQTT       ┌─────────────────────────────┐
│ Home         │◄───────────────►│ Caméra Qiara                │
│ Assistant    │  auto-discovery │                             │
│ + Alarmo     │                 │  openqiarad ──► MCU         │
└──────────────┘                 │            (UART, 868 MHz)  │
                                 └─────────────────────────────┘
```

`openqiarad` est lui-même la passerelle radio : le daemon constructeur
`fbxhome` est arrêté au boot, et `openqiarad` pilote directement le MCU
(appairage, heartbeat, crypto, bytecode des capteurs). Au premier démarrage, il
importe les capteurs déjà appairés depuis `/data/fbxhome.xml.*` en gardant
leurs IDs.

## Statut

**⚠️ Alpha — pas prêt pour la production.**

OpenQiara a été construit et validé sur les propres caméras de l'auteur, avec
son propre mix de capteurs. Chaque code path du tableau de statut ci-dessous
fonctionne de bout en bout *dans cette configuration*, mais le projet n'a pas
encore connu de déploiement plus large.

Attends-toi à des rugosités. Cas probablement non gérés :

- Modèles de capteurs / versions de firmware différents du hardware de l'auteur
  (le bytecode poussé à un capteur est identifié par préfixe de modèle ; une
  variante inconnue échouera silencieusement ou bouclera en reinit).
- Setups multi-caméras (le daemon suppose une seule caméra par réseau).
- Pairing concurrent de plusieurs capteurs du même type.
- Cas réseau limites : broker MQTT offline au boot, conflits mDNS, réseaux
  IPv6-only, WiFi à portail captif.
- Stabilité long terme : validé sur quelques heures/jours, pas des semaines.
  Une fuite mémoire dans le pipeline HomeKit + SRTP est plausible.
- Mises à jour firmware caméra poussées par Free (aucune observée depuis le
  shutdown, mais le bootloader appelle toujours la maison s'il est joignable).

Si tu tombes sur un cas non couvert, ouvre une issue avec les logs pertinents
de `/data/openqiarad.log`. Les PR sont les bienvenues.

| Fonctionnalité | Statut |
|---------|--------|
| Events détecteurs d'ouverture (DWS) | ✅ |
| Events mouvement PIR | ✅ |
| Clavier (KPD) PIN + armement/désarmement | ✅ (un seul PIN) |
| Déclenchement sirène (SRN) | ✅ (son discret ; voir limitations pour le wail pleine puissance) |
| Moteur d'alarme autonome | ✅ (source Local/Remote, grâce post-armement) |
| Mode bridge Alarmo | ✅ |
| Auto-discovery MQTT pour HA | ✅ |
| Bridge HomeKit (capteurs + alarme) | ✅ |
| **Vidéo live caméra HomeKit** | ✅ (SRTP pure-Go natif, sans ffmpeg) |
| Audio caméra HomeKit | ✅ AAC-ELD (libfdk-aac de la caméra) |
| Web UI pour appairage & config | ✅ |
| Batterie | ⚠️ niveau réel pour la sirène seulement |
| Température | ❌ |

## Matériel supporté

| Appareil | Préfixe modèle | Entité HA |
|--------|--------------|-----------|
| Détecteur d'ouverture | `HOMELABDWS` | `binary_sensor` (opening) |
| Détecteur de mouvement | `HOMELABPIR` | `binary_sensor` (motion) |
| Sirène | `HOMELABSRN` | `siren` |
| Clavier | `HOMELABKPD` | `alarm_control_panel` |
| Caméra | `HOMELABCAM` | (appareil hôte) |

OpenQiara ne peut pas appairer de capteurs neufs d'autres fabricants — seuls
les capteurs livrés à l'origine avec un kit Qiara fonctionnent.

## Limitations connues

Voici les rugosités identifiées. Elles sont pour la plupart inhérentes au fait
de tourner offline sur un firmware constructeur conçu pour un cloud désormais
mort — pas des bugs à corriger en bidouillant `openqiarad`.

- **PIN clavier.** Un seul PIN par clavier. Ajouter un second code via l'API
  stock nécessite une autorisation cloud et échoue silencieusement en offline.

- **Sirène après remise sous tension physique.** Débrancher puis rebrancher le
  SRN peut le laisser `reachable=1` mais muet, ou complètement injoignable.
  Récupération : remise sous tension physique complète. Voir
  [`docs/sensors.md`](docs/sensors.md).

- **Sécurité — état alpha.** Le mot de passe admin du Web UI est stocké
  en clair dans `/data/openqiara.json` et la comparaison HTTP Basic Auth
  n'est pas constant-time. CORS est ouvert (`*`) et il n'y a pas de
  protection CSRF ni de rate limiting. À considérer comme un LAN de
  confiance uniquement ; ne pas exposer la caméra sur Internet. Voir
  [`SECURITY.md`](SECURITY.md).

## Démarrage rapide

**Prérequis :** macOS avec [Homebrew](https://brew.sh), Linux (Ubuntu/Debian)
**ou** Windows avec WSL2 et [usbipd-win](https://github.com/dorssel/usbipd-win),
et un réseau WiFi 2.4 GHz. Trois scripts d'install équivalents sont fournis :
`sd_setup.sh` (macOS), `sd_setup_ubuntu.sh` (Linux) et `sd_setup_wsl.sh`
(Windows via WSL2).

### 1. Sauvegarde de la carte SD

Avant toute chose, sauvegarde la carte SD d'origine. Éteins la caméra, retire
la SD, insère-la dans ton Mac, et trouve son identifiant de disque :

```bash
diskutil list   # cherche le disque avec 3 partitions Linux (ex : disk4)
```

Puis dump les trois partitions :

```bash
DISK=disk4   # ajuster selon ton disque
sudo dd if=/dev/r${DISK}s1 bs=1M of=backup_rootfs.img
sudo dd if=/dev/r${DISK}s2 bs=1M of=backup_data.img
sudo dd if=/dev/r${DISK}s3 bs=1M of=backup_media.img
```

Sur **Linux**, repère le disque avec `lsblk` et dumpe les partitions
(`sdb1`/`sdb2`/`sdb3` sur un lecteur USB, `mmcblk0p1`… sur un contrôleur SD) :

```bash
DISK=sdb   # ajuster (lsblk)
sudo dd if=/dev/${DISK}1 bs=1M of=backup_rootfs.img
sudo dd if=/dev/${DISK}2 bs=1M of=backup_data.img
sudo dd if=/dev/${DISK}3 bs=1M of=backup_media.img
```

Sur **Windows**, passe par WSL2. Son noyau lit et écrit l'ext4, mais il ne
voit pas les lecteurs de Windows : il faut lui confier le lecteur de carte
avec usbipd. Dans un PowerShell **administrateur** :

```powershell
winget install usbipd
usbipd list
```

Repère ton lecteur de carte dans la liste, souvent affiché comme
« Dispositif de stockage de masse USB » (« USB Mass Storage Device » sur un
Windows en anglais). En cas de doute, compare la liste lecteur débranché puis
branché. Note son `BUSID` (ex : `4-8`) et attache-le à WSL :

```powershell
usbipd bind --busid 4-8           # une seule fois, en administrateur
usbipd attach --wsl --busid 4-8   # après chaque redémarrage de WSL ou du lecteur
usbipd list                       # la colonne STATE doit afficher "Attached"
```

> usbipd ne partage que des périphériques USB. Un lecteur SD intégré au
> portable et branché en PCIe n'apparaît pas dans `usbipd list` : utilise
> alors un lecteur USB. Tant qu'il est attaché à WSL, le lecteur disparaît
> de Windows.

Dans WSL, installe les outils (une seule fois) puis repère la carte. Le
client `usbip` (`linux-tools-generic`) que citent d'anciens guides est
inutile : usbipd-win 4+ embarque le sien.

```bash
sudo apt-get update && sudo apt-get install -y e2fsprogs file
sudo modprobe usb_storage   # WSL ne le charge pas toujours à l'attach
lsblk -o NAME,SIZE,TYPE,MODEL
```

Les premiers disques (`sda` à `sdd`, modèle `Virtual Disk`) appartiennent à
WSL et n'ont pas de partitions. La carte est le disque suivant, avec trois
partitions (ex : `sde` avec `sde1`/`sde2`/`sde3`). Dumpe-les depuis ton home
WSL, bien plus rapide que `/mnt/c` :

```bash
cd ~
DISK=sde   # ajuster (lsblk)
sudo dd if=/dev/${DISK}1 bs=1M of=backup_rootfs.img
sudo dd if=/dev/${DISK}2 bs=1M of=backup_data.img
sudo dd if=/dev/${DISK}3 bs=1M of=backup_media.img
```

Laisse le lecteur attaché : l'étape 3 en a besoin.

Garde ces fichiers en lieu sûr — c'est ton billet retour si quelque chose tourne mal.

### 2. Télécharger les artefacts

Récupère le tag de la dernière release sur [la page des releases](https://github.com/Caligone/openqiara/releases)
(tant qu'on est en pre-release alpha, l'URL `releases/latest/download/` renvoie un 404 — il faut
pointer le tag explicitement) :

```bash
mkdir -p openqiara && cd openqiara

VERSION=v0.1.0-alpha.3   # ajuster selon la dernière release publiée

# Binaire daemon (ARMv7) + scripts d'install
BASE=https://github.com/Caligone/openqiara/releases/download/$VERSION
curl -LO $BASE/openqiarad-linux-arm7
curl -LO $BASE/sd_setup.sh          # macOS ; Linux : sd_setup_ubuntu.sh ; Windows (WSL2) : sd_setup_wsl.sh
curl -LO $BASE/camera_boot.sh   # requis : sd_setup.sh le copie dans /data/boot.sh
curl -LO $BASE/SHA256SUMS

# Vérification d'intégrité
shasum -a 256 -c SHA256SUMS

chmod +x sd_setup.sh                # ou sd_setup_ubuntu.sh / sd_setup_wsl.sh
```

> Les scripts `sd_setup*.sh` cherchent `camera_boot.sh` à côté d'eux (install
> release-only) ou dans `scripts/` (checkout du repo). Garde-les dans le même
> dossier. Sous WSL, travaille dans ton home WSL plutôt que sous `/mnt/c`.

Si tu préfères compiler toi-même, voir la section [Compilation](#compilation) plus bas.

### 3. Préparer la carte SD

**macOS :**

```bash
brew install e2fsprogs

./sd_setup.sh \
    --disk disk4 \
    --wifi-ssid "MonReseau" \
    --wifi-pass "MonMotDePasse" \
    --daemon openqiarad-linux-arm7 \
    --ssh-pubkey ~/.ssh/id_ed25519.pub
```

**Linux (Ubuntu/Debian) :**

```bash
sudo apt install e2fsprogs   # généralement déjà présent

# --disk = nom du périphérique bloc sans /dev/ (repère-le avec: lsblk)
#   lecteur USB : sdb   |   contrôleur eMMC/SD : mmcblk0
sudo ./sd_setup_ubuntu.sh \
    --disk sdb \
    --wifi-ssid "MonReseau" \
    --wifi-pass "MonMotDePasse" \
    --daemon openqiarad-linux-arm7 \
    --ssh-pubkey ~/.ssh/id_ed25519.pub
```

**Windows (WSL2) :**

Le lecteur doit être attaché à WSL (voir l'étape 1). Le script appelle
`sudo` lui-même : lance-le sans `sudo`.

```bash
# --disk = le disque repéré à l'étape 1 avec lsblk (ex : sde)
./sd_setup_wsl.sh \
    --disk sde \
    --wifi-ssid "MonReseau" \
    --wifi-pass "MonMotDePasse" \
    --daemon openqiarad-linux-arm7 \
    --ssh-pubkey ~/.ssh/id_ed25519.pub
```

Rends ensuite le lecteur à Windows avant de retirer la carte (PowerShell) :

```powershell
usbipd detach --busid 4-8
```

> **`--ssh-pubkey` n'est pas optionnel en pratique.** La caméra n'autorise
> aucun mot de passe et n'embarque aucune clé : l'accès SSH vient uniquement
> de `/data/ssh_authorized_keys`, écrit par cette commande. Sans clé, la
> caméra démarre et l'interface web répond, mais `ssh` renvoie
> `Permission denied` — et la seule façon d'en ajouter une ensuite est de
> refaire la carte SD.

### 4. Démarrage

Réinsère la SD dans la caméra, allume-la, et attends ~60 secondes.
Trouve la caméra sur le réseau :

```bash
arp -a | grep lwip
```

Sous Windows, `arp -a` n'affiche pas les noms d'hôte, et depuis WSL il ne
voit pas ton réseau local. Ouvre `http://openqiara.local` dans un navigateur
Windows, ou cherche la caméra dans la liste des appareils de ta box.

> **Caméra introuvable sur le réseau ?** Le boot écrit un diagnostic wifi
> dans `/data/boot_wifi.log` (écrasé à chaque démarrage). Tu peux le lire
> en remettant la SD dans le Mac et en ouvrant la partition `data` avec
> `debugfs -R "cat boot_wifi.log" /dev/diskNs2`. Sous WSL, rattache le
> lecteur et monte la partition en lecture seule :
> `sudo mount -o ro /dev/sdX2 /mnt && cat /mnt/boot_wifi.log; sudo umount /mnt`.
> Tu y verras le SSID
> configuré, l'état de l'interface `ssv0` et les messages `dmesg` du
> driver wifi. Rappels : la puce est **2.4 GHz uniquement** et ne gère
> que WPA2-PSK (pas WPA3-only ni WPA-Enterprise).

> **`Permission denied (publickey)` en SSH ?** Deux causes, dans cet ordre :
>
> 1. **Aucune clé n'a été installée.** Vérifie depuis la SD :
>    `debugfs -R "cat ssh_authorized_keys" /dev/diskNs2` (macOS) ou
>    `sudo debugfs -R "cat ssh_authorized_keys" /dev/sdX2` (Linux, WSL). Si le
>    fichier est vide ou absent, relance `sd_setup.sh` avec `--ssh-pubkey`.
>    Attention : le script **reformate `/data`**, donc tu repars d'une config
>    vierge et les capteurs sont à ré-appairer. Sur une caméra déjà en
>    service, préfère écrire la clé directement dans la partition :
>    `debugfs -w -R "write ~/.ssh/id_ed25519.pub ssh_authorized_keys" /dev/diskNs2`.
>    Sous WSL, le noyau monte l'ext4 : copie simplement la clé dans la
>    partition, comme le fait `sd_setup_wsl.sh` :
>    `sudo mount /dev/sdX2 /mnt && sudo cp ~/.ssh/id_ed25519.pub /mnt/ssh_authorized_keys && sudo umount /mnt`.
> 2. **Ta clé est au format RSA.** Le dropbear de la caméra ne signe qu'en
>    `ssh-rsa` (SHA-1), qu'OpenSSH 8.7+ refuse par défaut. Ajoute
>    `-o PubkeyAcceptedAlgorithms=+ssh-rsa`, ou utilise une clé ed25519 qui
>    n'a pas ce problème.

### 5. Configuration

Ouvre `http://openqiara.local` (ou l'IP de la caméra) pour appairer les
capteurs et configurer MQTT.

## Compilation

Si tu préfères builder toi-même plutôt que d'utiliser une release :

```bash
git clone https://github.com/Caligone/openqiara
cd openqiara

# Cross-compile le daemon pour la caméra (ARMv7)
# zig (brew install zig) compile l'encodeur audio AAC-ELD contre la glibc
# de la caméra. Sans zig ni CGO_ENABLED=1 : même binaire, sans le son.
CGO_ENABLED=1 CC="zig cc -target arm-linux-gnueabihf.2.30" \
GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o bin/openqiarad ./cmd/openqiarad

# Tests (à lancer sur ta machine de dev)
go test ./...
```

**Prérequis** : [Go 1.26+](https://go.dev/dl/).

## Documentation

- [`docs/getting-started.md`](docs/getting-started.md) — commence ici
- [`docs/install.md`](docs/install.md) — installation avancée (préparation SD, recovery)
- [`docs/architecture.md`](docs/architecture.md) — architecture interne de `openqiarad`
- [`docs/sensors.md`](docs/sensors.md) — appairage et comportement par capteur
- [`docs/kpd.md`](docs/kpd.md) — détails du protocole clavier (PIN, armement/désarmement, particularités)
- [`docs/mqtt.md`](docs/mqtt.md) — topics MQTT et discovery Home Assistant
- [`docs/homekit.md`](docs/homekit.md) — intégration HomeKit
- [`docs/api.md`](docs/api.md) — API HTTP locale (`/api/v1`)
- [`docs/openapi.json`](docs/openapi.json) — la même en OpenAPI 3.1 : génération de client, Postman, Bruno
- [`docs/protocol.md`](docs/protocol.md) — **référence protocole DomusRF** (pour contributeurs et curieux)
- [`CONTRIBUTING.md`](CONTRIBUTING.md) — règles de contribution : CI, commits, synchronisation de l'API, conventions de rétro-ingénierie
- [`SECURITY.md`](SECURITY.md) — politique de sécurité et report de vulnérabilités

## Quel rapport avec le firmware Qiara ?

OpenQiara est un **overlay** sur le rootfs stock de la caméra. Il **ne**
re-flashe **pas** la caméra, ne remplace pas le kernel, et ne touche pas au
firmware du MCU. Il se lance à la place du daemon original
`fbxhome` (arrêté au boot), parle au même multiplexeur UART `charmux` que le
firmware d'origine, et utilise le même protocole DomusRF avec les mêmes capteurs.

Ce qui reste du constructeur :

- Kernel Linux + drivers Sigmastar (le SoC de la caméra)
- Driver WiFi (`ssv6x5x.ko`)
- `charmux` (multiplexeur UART vers le MCU)
- `uartboot` (flashe le firmware MCU à chaque boot)
- Le firmware MCU lui-même (`/lib/firmwares/hlcam02_ctrl.bin`)
- Encodeur vidéo (`hlcamd`)

Ce qu'OpenQiara remplace :

- Le daemon applicatif `fbxhome` — reverse-engineeré et réimplémenté en Go,
  sans dépendance cloud

C'est similaire en esprit à **Tasmota sur ESP** ou **OpenWrt sur routeurs** :
le bootloader et les blobs propriétaires restent, la couche applicative
devient du logiciel libre.

## Aspects légaux

Distribué sous [Licence MIT](LICENSE).

OpenQiara est un projet d'**interopérabilité** au titre de la directive
européenne 2009/24/CE (Article 6) et du CPI français L122-6-1.IV. Aucun code
propriétaire, firmware ou matériel cryptographique de Qiara, Free, Iliad,
Cofidur EMS ou Sigmastar n'est inclus dans ce dépôt ni distribué par lui.

**Utilisation à tes risques et périls.** Modifier ta caméra peut annuler la
garantie constructeur (mais Qiara étant défunte, c'est surtout théorique) et
peut briquer l'appareil si tu ignores les avertissements sur les opcodes MCU
dangereux dans [`docs/protocol.md`](docs/protocol.md). Fais des backups.

## Remerciements

L'essentiel de cette base de code — le reverse engineering DomusRF, le daemon
Go, le pipeline de streaming caméra HomeKit (multicast hlcamd → packetizer
RTP H.264 et AAC-ELD → SRTP) — a été écrit en sessions de pair-programming
avec [Claude Code](https://www.anthropic.com/claude-code) tournant sur le
modèle **Claude Opus**. Les décisions d'architecture, la validation sur le
hardware réel, et les innombrables investigations en cul-de-sac ont été une
collaboration humain + LLM. Le résultat est le travail d'un hobbyiste solo
qui n'aurait jamais sorti tout ça en temps raisonnable seul.
