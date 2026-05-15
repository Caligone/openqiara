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
│ + Alarmo     │                 │  openqiarad ──► fbxhome ──► │
└──────────────┘                 │                  │       MCU│
                                 │              (charmux UART) │
                                 │              radio 868 MHz  │
                                 └─────────────────────────────┘
```

Par défaut, `openqiarad` tourne en **mode proxy `fbxhome`** : le daemon
constructeur `fbxhome` reste en charge de la radio (cycle de vie des capteurs,
crypto MCU), et `openqiarad` discute avec son API HTTP locale + suit son fichier
de log. Un mode `charmux` direct (sans `fbxhome`) existe également, mais ce
n'est pas la voie recommandée aujourd'hui — on y perd la crypto MCU de fbxhome
et la gestion du heartbeat.

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
de `/data/openqiarad.log` et `/var/log/fbxhome.log`. Les PR sont les bienvenues.

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
| Audio caméra HomeKit | ⚠️ silencieux (transcodeur CGo libfdk-aac à venir) |
| Web UI pour appairage & config | ✅ |
| Reporting batterie / température | ✅ |

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

- **Volume du wail d'intrusion sirène.** Les endpoints `test_power` et
  `test_duration` sont en lecture seule via l'API locale fbxhome, donc le
  "wail" qu'on déclenche en intrusion est le même son discret que le bouton de
  test manuel (~10 s, faible puissance). Un vrai wail pleine puissance
  nécessiterait de pousser fbxhome dans son état interne `alarm_trigged` (ou de
  parler directement au SRN en mode charmux) — ni l'un ni l'autre n'est fait
  aujourd'hui.

- **Sirène après remise sous tension physique.** Débrancher puis rebrancher le
  SRN peut le laisser `reachable=1` mais muet, ou complètement injoignable.
  Récupération : `fbxbusctl call fbxhome reboot_srn` pour le cas muet, remise
  sous tension physique complète si totalement injoignable. Voir
  [`docs/sensors.md`](docs/sensors.md).

- **Patch binaire constructeur requis.** Pour utiliser `openqiarad` comme seul
  contrôleur d'alarme (surtout en mode `alarmo`), on patche deux instructions
  dans `/usr/bin/fbxhome` pour que le KPD cesse de piloter en parallèle la
  machine à états d'alarme interne de fbxhome. Le patch est appliqué au boot
  depuis `/data/fbxhome.patched`. Détails et offsets dans
  [`docs/protocol.md`](docs/protocol.md) (§ "fbxhome binary patch").

- **Audio caméra HomeKit.** La vidéo marche, l'audio est silencieux. HomeKit
  exige de l'AAC-ELD ; le pipeline pure-Go ne le transcode pas encore.

- **Sécurité — état alpha.** Le mot de passe admin du Web UI est stocké
  en clair dans `/data/openqiara.json` et la comparaison HTTP Basic Auth
  n'est pas constant-time. CORS est ouvert (`*`) et il n'y a pas de
  protection CSRF ni de rate limiting. À considérer comme un LAN de
  confiance uniquement ; ne pas exposer la caméra sur Internet. Voir
  [`SECURITY.md`](SECURITY.md).

## Démarrage rapide

**Prérequis :** macOS avec [Go 1.26+](https://go.dev/dl/) et
[Homebrew](https://brew.sh). Un réseau WiFi 2.4 GHz.

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

Garde ces fichiers en lieu sûr — c'est ton billet retour si quelque chose tourne mal.

### 2. Build et installation

```bash
git clone https://github.com/Caligone/openqiara
cd openqiara

# Installer les dépendances
brew install e2fsprogs

# Compiler le daemon pour la caméra (ARMv7)
GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o bin/openqiarad ./cmd/openqiarad

# Préparer la carte SD
./scripts/sd_setup.sh \
    --disk disk4 \
    --wifi-ssid "MonReseau" \
    --wifi-pass "MonMotDePasse" \
    --ssh-pubkey ~/.ssh/id_ed25519.pub
```

### 3. Démarrage

Réinsère la SD dans la caméra, allume-la, et attends ~60 secondes.
Trouve la caméra sur le réseau :

```bash
arp -a | grep lwip
```

Ouvre `http://openqiara.local` (ou l'IP de la caméra) pour appairer les
capteurs et configurer MQTT.

## Compilation

```bash
# Cross-compile le daemon pour la caméra (ARMv7)
GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o bin/openqiarad ./cmd/openqiarad

# Tests (à lancer sur ta machine de dev)
go test ./...
```

## Documentation

- [`docs/getting-started.md`](docs/getting-started.md) — commence ici
- [`docs/install.md`](docs/install.md) — installation avancée (préparation SD, recovery)
- [`docs/architecture.md`](docs/architecture.md) — architecture interne de `openqiarad`
- [`docs/sensors.md`](docs/sensors.md) — appairage et comportement par capteur
- [`docs/kpd.md`](docs/kpd.md) — détails du protocole clavier (PIN, armement/désarmement, particularités)
- [`docs/mqtt.md`](docs/mqtt.md) — topics MQTT et discovery Home Assistant
- [`docs/homekit.md`](docs/homekit.md) — intégration HomeKit
- [`docs/protocol.md`](docs/protocol.md) — **référence protocole DomusRF** (pour contributeurs et curieux)
- [`SECURITY.md`](SECURITY.md) — politique de sécurité et report de vulnérabilités

## Quel rapport avec le firmware Qiara ?

OpenQiara est un **overlay** sur le rootfs stock de la caméra. Il **ne**
re-flashe **pas** la caméra, ne remplace pas le kernel, et ne touche pas au
firmware du MCU. Il se lance à côté (ou à la place) du daemon original
`fbxhome`, parle au même multiplexeur UART `charmux` que le firmware d'origine,
et utilise le même protocole DomusRF avec les mêmes capteurs.

Ce qui reste du constructeur :

- Kernel Linux + drivers Sigmastar (le SoC de la caméra)
- Driver WiFi (`ssv6x5x.ko`)
- `charmux` (multiplexeur UART vers le MCU)
- `uartboot` (flashe le firmware MCU à chaque boot)
- Le firmware MCU lui-même (`/lib/firmwares/hlcam02_ctrl.bin`)
- Pipeline vidéo (`hlcamd`, `hls`)

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
Go, le pipeline de streaming caméra HomeKit (HLS watcher → parser MPEG-TS →
packetizer RTP H.264 → SRTP) — a été écrit en sessions de pair-programming
avec [Claude Code](https://www.anthropic.com/claude-code) tournant sur le
modèle **Claude Opus**. Les décisions d'architecture, la validation sur le
hardware réel, et les innombrables investigations en cul-de-sac ont été une
collaboration humain + LLM. Le résultat est le travail d'un hobbyiste solo
qui n'aurait jamais sorti tout ça en temps raisonnable seul.
