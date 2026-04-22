# Architecture

## Vue d'ensemble

OpenQiara est un **overlay** sur le rootfs stock de la caméra. Il ne re-flashe
pas le firmware. Le daemon constructeur `fbxhome` reste en charge de la radio
868 MHz et de la crypto MCU ; `openqiarad` le proxifie pour exposer les
capteurs à Home Assistant / HomeKit, et fait tourner le moteur d'alarme local
(ou le bridge vers l'Alarmo de Home Assistant).

```
┌──────────────────────────────────────────────────────────────┐
│ Caméra Qiara                                                 │
│                                                              │
│  ┌────────────┐ HTTPS  ┌─────────┐ charmux  ┌──────────────┐ │
│  │ openqiarad │◄──────►│ fbxhome │◄────────►│ MCU EZR32LG  │ │
│  │            │ :64218 │ (vendor)│  UART    │ radio Si446x │ │
│  │  ┌──────┐  │        │ patché  │          │  868MHz      │ │
│  │  │Web UI│  │ tail   │         │          └──────────────┘ │
│  │  └──────┘  │◄──────fbxhome.log│                           │
│  └─────┬──────┘        └─────────┘                           │
│        │ :80                                                 │
└────────┼─────────────────────────────────────────────────────┘
         │ WiFi
         ▼
┌─────────────────┐     ┌──────────────────┐
│ Navigateur      │     │ Home Assistant   │
│ (setup/pairing) │     │ MQTT discovery   │
└─────────────────┘     │ Alarmo (optionnel)│
                        │ Bridge HomeKit   │
                        └──────────────────┘
```

Le binaire fbxhome est patché (2 NOPs) pour découpler le KPD de la machine à
états d'alarme interne de fbxhome — voir
[`protocol.md`](protocol.md) § "fbxhome binary patch".

## Modes caméra

OpenQiara peut parler au MCU de deux façons, sélectionnées au démarrage via
`-mode fbxhome|charmux|auto`. Le mode recommandé (et le plus testé) est
**fbxhome**.

### Mode fbxhome (par défaut, recommandé)

OpenQiara utilise le binaire fbxhome existant comme proxy :

```
openqiarad → HTTPS [::1]:64218 → fbxhome → charmux → MCU
            (api/v1/home/*)
```

Appairage via l'API fbxhome. Events capteurs en temps réel via tail de
`/var/log/fbxhome.log` (package `internal/fbxhomelog`), avec un filet de
sécurité polling à 5 min. Un polling agressif à 2s a été identifié le
2026-05-13 comme cause de saturation de fbxhome (cascade 502 nginx +
instabilité KPD post-batterie), d'où l'architecture tail-first.

Compromis : conserve la gestion crypto MCU, la logique heartbeat, et le flow
de push de bytecode de fbxhome. On perd un peu de contrôle bas niveau sur la
radio.

### Mode charmux (legacy, debug)

OpenQiara parle directement au MCU via les sockets UDP charmux :

```
openqiarad → UDP 8000/8002 → charmux → MCU
```

Supprime la dépendance à fbxhome. Appairage réimplémenté en Go (handshake
9 phases, crypto AES-128-OCB3). Utile pour l'investigation radio bas niveau
mais **pas la voie de production** — fbxhome est plus éprouvé pour le cycle
de vie des capteurs.

### Remplacement constructeur complet (futur)

Supprimer fbxhome entièrement (flashage firmware MCU via uartboot pure-Go,
toute la gestion capteurs dans `openqiarad`) est un objectif long terme. Pas
sur la roadmap court terme.

## Layout rootfs

La carte SD garde ses 3 partitions ; le rootfs stock est largement inchangé.
`openqiarad` vit sur `/data` et est démarré par `boot.sh`, qui est crocheté
dans l'init existant.

| Partition | Montage | Contenu |
|-----------|-------|---------|
| mmcblk0p1 | / | Rootfs stock constructeur (kernel, drivers, fbxhome, charmux, uartboot, hlcamd, nginx) |
| mmcblk0p2 | /data | Binaire openqiarad, config persistante, état capteurs, état moteur d'alarme, log |
| mmcblk0p3 | /media | Stockage média (segments HLS, etc.) |

### Ce qu'on garde du stock
- Kernel Linux 4.9.84
- Modules kernel (WiFi `ssv6x5x`, capteurs caméra, etc.)
- Firmware MCU (`hlcam02_ctrl.bin`) flashé à chaque boot par `uartboot`
- Multiplexeur UART `charmux`
- `fbxhome` — gardé en production pour la radio + crypto MCU + push bytecode.
  Patché (2 NOPs) pour qu'il arrête de piloter sa propre machine à états
  d'alarme interne sur les events KPD ; voir `protocol.md`.
- `hlcamd` + `hls` pour le pipeline vidéo
- `nginx` (écoute toujours sur :64218 pour l'API HTTPS fbxhome)
- SSH `dropbear`

### Ce qu'on ajoute
- `openqiarad` sur `/data` — le daemon qui chapeaute tout : discovery MQTT
  pour HA, bridge HomeKit, moteur d'alarme, web UI sur `:80`.
- `boot.sh` sur `/data` — applique le patch fbxhome au boot, démarre le
  daemon, gère le watchdog.

OpenQiara ne remplace **pas** le système init, `fbxhome`, `hlconnman`, ni
`nginx`. Il coexiste avec eux.

## Structure du module Go

```
openqiara/
├── cmd/
│   ├── openqiarad/         # Daemon principal (tourne sur la caméra)
│   ├── openqiara-flash/    # Outil de flash SD (poste de dev)
│   ├── mcu-info/           # Lecteur d'info MCU (debug)
│   ├── charmux-test/       # Client charmux debug (debug)
│   ├── decode-frame/       # Décodeur de managed frame (debug)
│   ├── fbxbus-poc/         # PoC protocole fbxbus (debug)
│   └── rtptest/            # Test pipeline SRTP/RTP (debug)
├── internal/
│   ├── alarm/              # Machine à états d'alarme autonome
│   │   └── engine.go       # Transitions d'état, armement Source-aware, timers, persistance
│   ├── camera/             # Interface client caméra + implémentations
│   │   ├── client.go       # Interface Client (Sensors, Pair, Events, SendPKT, SetShutter, TriggerSiren)
│   │   ├── fbxhome.go      # FbxhomeClient (proxy HTTPS — voie de production)
│   │   ├── charmux_client.go # CharmuxClient (MCU direct — debug / legacy)
│   │   ├── logwatcher.go   # Watcher d'events KPD sur fbxhome.log (kpd_id dynamique)
│   │   ├── kpdcodes.go     # Gestion XML des codes KPD
│   │   └── types.go        # Sensor, SensorEvent, etc.
│   ├── charmux/            # Client UDP charmux bas niveau
│   ├── fbxbus/             # Client IPC fbxbus (protocole binaire type DBus)
│   ├── fbxhomelog/         # Tail de /var/log/fbxhome.log → events capteurs (temps réel)
│   ├── config/             # Store de config JSON
│   ├── domus/              # Handshake d'appairage DomusRF (mode charmux)
│   ├── mdns/               # Annonce mDNS (openqiara.local)
│   ├── mqtt/               # Publisher MQTT HA + discovery
│   ├── publisher/          # Abstraction Publisher (MQTT + HomeKit en parallèle)
│   ├── system/             # Helpers système (reboot, etc.)
│   └── web/                # Web UI + API REST + SSE
│       └── server.go       # Handlers HTTP, auth, proxy HLS, pipeline SRTP
└── web/
    └── static/
        └── index.html      # SPA (embarquée via embed.FS)
```

## Moteur d'alarme

La machine à états d'alarme autonome vit dans `internal/alarm/engine.go`.
Elle tourne dans deux modes :

- **`standalone`** : engine local, source de vérité de l'état alarme
- **`alarmo`** : engine local désactivé, HA Alarmo via MQTT est la source
  de vérité ; openqiarad écoute `alarmo/state` et publie sur
  `alarmo/command`

États : `disarmed`, `arming`, `armed_away`, `armed_night`, `pending`,
`triggered`.

### Armement Source-aware

Les commandes portent une `Source` (`Local` ou `Remote`) qui adapte le comportement :

- **`SourceLocal`** (KPD physique) : `disarmed → arming` pendant
  `arming_delay_seconds`, puis `arming → armed_*`. Pendant le délai,
  les events capteurs sont ignorés (grâce pour fermer la porte/sortir).
  À expiration, snapshot : tout capteur encore en alarme déclenche
  immédiatement `pending`.

- **`SourceRemote`** (HK / HA Alarmo / Web UI) : pas de délai d'armement
  (`disarmed → armed_*` direct), check immédiat post-arm : tout capteur
  déjà en alarme déclenche `pending` instantanément.

Cette asymétrie reflète l'usage : Local = user présent qui peut réagir,
Remote = user distant qui veut une protection stricte immédiate.

### Flow de déclenchement

```
armed_*  ──(alarme capteur)──►  pending  ──(timer)──►  triggered  ──(timer)──►  armed_*
   ▲                              │                        │
   └────────(désarmement à tout moment)─┴───────────────────┘
```

`pending` est la temporisation de grâce user (code clavier OK = disarm avant
`pending → triggered`). `triggered` joue le wail SRN pendant
`wail_duration_seconds`, puis retour à l'état armé précédent.

## Pattern Publisher

Les publishers synchronisent l'état avec les systèmes externes. Plusieurs publishers fonctionnent en parallèle :

```
                     ┌─── MQTTPublisher ──→ HA (broker MQTT)
Capteurs → Core ────┤
                     └─── HomeKitPublisher ──→ Apple Home / HA (mDNS)
```

Interface :
```go
type Publisher interface {
    Start(ctx, sensors, commands) error
    PublishSensorState(ctx, sensor) error
    PublishAlarmState(ctx, state) error
    Close() error
}
```

Le `CommandHandler` reçoit les commandes entrantes (arm/disarm, siren on/off) de n'importe quel publisher et les dispatch à tous les autres.

## API REST

| Méthode | Endpoint | Description |
|---------|----------|-------------|
| GET | /api/status | Uptime, statut MQTT, nombre de capteurs |
| GET | /api/config | Config complète (lecture) |
| PUT | /api/config/alarm | Modifier le mode alarme + délais + sons sirène |
| PUT | /api/config/web | Modifier la config web |
| GET | /api/sensors | Liste des capteurs avec état |
| POST | /api/sensors/pair | Démarrer un appairage |
| GET | /api/sensors/pair | Poller l'appairage |
| DELETE | /api/sensors/pair | Annuler l'appairage |
| PUT | /api/sensors/{id} | Renommer un capteur |
| DELETE | /api/sensors/{id} | Supprimer un capteur |
| GET | /api/alarm | État alarme courant |
| POST | /api/alarm | Changer l'état alarme (`{"action":"arm_away\|arm_night\|disarm"}`) |
| GET | /api/codes | Lister les codes KPD |
| POST | /api/codes | Ajouter un code |
| DELETE | /api/codes | Supprimer un code |
| POST | /api/siren/test | Tester la sirène (son discret) |
| POST | /api/siren/alarm_test | Tester le wail d'intrusion |
| POST | /api/shutter | Ouvrir/fermer le cache |
| POST | /api/stream/start | Activer le flux HLS |
| GET | /stream/* | Servir les segments HLS |
| GET | /api/events | Server-Sent Events (état alarme, capteurs temps réel) |
| POST | /api/reboot | Redémarrer la caméra |
| POST | /api/debug/pkt | Envoyer un paquet PKT brut |
