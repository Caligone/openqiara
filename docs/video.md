# Vidéo

## Architecture

```
hlcamd (encodeur H.264 hardware + micro)
   │  multicast local 224.0.0.1 : :9600 H.264 1080p, :9700 PCM 16 kHz
   ▼
openqiarad (mediahub) ──→ HomeKit (SRTP, AAC-ELD)
                     ├─→ RTSP :8554 (H.264 + AAC-LC)
                     └─→ HLS /stream/ (MPEG-TS, H.264 + AAC-LC) → navigateur, VLC, Home Assistant
```

Le SoC Sigmastar encode en H.264 via son encodeur hardware (`hlcamd --use-h264 --no-720p --no-360p`, cf [`../scripts/camera_boot.sh`](../scripts/camera_boot.sh)) : seul le flux principal 1080p sert, couper les deux autres encodages économise environ un quart de cœur et 14 Mo. `hlcamd` envoie ses flux en multicast sur le loopback ; openqiarad les lit et sert lui-même le HLS. Le segmenteur `hls` du constructeur ne tourne plus, sauf avec la source de secours `homekit.camera.source: "hls"`.

## Activation du flux

Le flux démarre à la demande : la première requête HLS, RTSP ou HomeKit lance le pipeline, qui s'arrête quand plus personne ne regarde. Le clapet fermé met `hlcamd` en pause : aucune image ne sort tant qu'il reste fermé.

```bash
# Ouvre le clapet et relance hlcamd
POST http://<camera>/api/v1/commands/stream/start
# Retourne: {"ok": true, "hls": "/stream/HLS_TEST.m3u8", "720": "/stream/720p/HLS_TEST.m3u8"}
```

## URLs HLS

| URL | Contenu |
|-----|---------|
| `http://<camera>/stream/HLS_TEST.m3u8` | 1080p + son |
| `http://<camera>/stream/720p/HLS_TEST.m3u8` | le même flux (URL historique de `hls`, gardée) |

## Segments

- Format : MPEG-TS (`.ts`), H.264 Constrained Baseline 1920×1080 30 i/s + AAC-LC 16 kHz mono
- Durée : ~1 s par segment (une IDR toutes les 0,5 s)
- Le muxer démarre à la première requête (~1-2 s avant le premier segment) et s'arrête 30 s après la dernière
- Latence : ~3-4 s (inhérente au HLS)

## Compatibilité

| Lecteur | Support |
|---------|---------|
| Safari (Mac/iOS) | ✅ Natif |
| Chrome/Firefox | ✅ (H.264 supporté) |
| VLC | ✅ |
| Apple Home (HomeKit) | ✅ via SRTP (voir [`homekit.md`](homekit.md)) |

## RTSP (recommandé pour NVR / détection)

Pour les consommateurs vidéo standard (Scrypted, Frigate, VLC, Home
Assistant), openqiarad expose un serveur **RTSP** natif : le 1080p de
`hlcamd` et le micro en **AAC-LC 16 kHz mono** (RFC 3640), encodé par
openqiarad avec la libfdk-aac de la caméra. La latence est bien plus faible
qu'en HLS (~5 s) : les NAL H.264 partent en RTP dès que `hlcamd` les émet.
Sans libfdk-aac, le flux est vidéo seule.

Activation dans `openqiara.json` :

```json
{
  "rtsp": {
    "enabled": true,
    "listen": ":8554",
    "path": "openqiara"
  }
}
```

URL : `rtsp://<camera>:8554/openqiara`

Le pipeline (mediahub) est partagé avec la sortie HomeKit et le HLS ; seul
le transport diffère. Le flux
démarre à la demande (première connexion RTSP) et s'arrête quand le dernier
client se déconnecte. Implémenté en Go pur via `bluenviron/gortsplib`, sans
ffmpeg.

## Shutter

Le cache objectif doit être ouvert pour voir l'image :

```
POST http://<camera>:8080/api/v1/commands/shutter
{"open": true}   # ouvrir
{"open": false}  # fermer
```

Le shutter est contrôlé via le canal charmux Shutter (port 8006).

## Limitations

- **Latence HLS ~3-4 s** : inhérente au protocole ; RTSP et HomeKit sont bien plus réactifs
- **Son** : demande la libfdk-aac de la caméra et un binaire compilé avec CGo (release, `make daemon`)
- **Shutter + hlcamd conflit** : hlcamd occupe le port Shutter 8007. openqiarad contourne en envoyant via UDP sans bind.
