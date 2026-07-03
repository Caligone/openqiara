# Vidéo — HLS Streaming

## Architecture

```
hlcamd (encodeur H.264/H.265 hardware) → hls (segmentation) → fichiers .ts
                                                                    ↓
openqiarad (file server /stream/ + HomeKit SRTP) → navigateur / Apple Home
```

Le SoC Sigmastar encode en H.264 ou H.265 via son encodeur hardware (`/dev/mi_venc`). Le processus `hls` stock est relancé avec `--use-h264` au boot (cf [`../scripts/camera_boot.sh`](../scripts/camera_boot.sh)) pour produire des segments MPEG-TS H.264 compatibles HomeKit et navigateurs.

## Activation du flux

Le flux HLS n'est pas actif par défaut. Il faut l'activer :

```bash
# Via fbxbus
fbxbusctl call hlcamd resume_streams

# Via l'API openqiarad
POST http://<camera>:8080/api/stream/start
# Retourne: {"ok": true, "hls": "/stream/HLS_TEST.m3u8", "720": "/stream/720p/HLS_TEST.m3u8"}
```

L'endpoint `/api/stream/start` ouvre aussi le shutter automatiquement.

## URLs HLS

| Résolution | URL |
|-----------|-----|
| Multi (adaptive) | `http://<camera>:8080/stream/HLS_TEST.m3u8` |
| 360p | `http://<camera>:8080/stream/360p/HLS_TEST.m3u8` |
| 720p | `http://<camera>:8080/stream/720p/HLS_TEST.m3u8` |
| 1080p | `http://<camera>:8080/stream/1080p/HLS_TEST.m3u8` |

## Segments

- Format : MPEG-TS (`.ts`)
- Codec : H.264 (avec `--use-h264` sur le process `hls`)
- Durée : ~1s par segment
- 4 segments dans la playlist (sliding window)
- Latence : ~4-5s (incompressible avec HLS)

## Compatibilité

| Lecteur | Support |
|---------|---------|
| Safari (Mac/iOS) | ✅ Natif |
| Chrome/Firefox | ✅ (H.264 supporté) |
| VLC | ✅ |
| Apple Home (HomeKit) | ✅ via SRTP (voir [`homekit.md`](homekit.md)) |

## RTSP (recommandé pour NVR / détection)

Pour les consommateurs vidéo standard (Scrypted, Frigate, VLC, Home
Assistant), openqiarad expose un serveur **RTSP** natif. Contrairement au
HLS, ce flux est **vidéo seule** (pas d'AAC) — la piste audio du HLS n'a
pas de global headers et casse le muxing RTSP chez la plupart des clients
(`AAC with no global headers is currently not supported`). La latence est
aussi bien plus faible (~1 s contre ~5 s en HLS), car les NAL H.264 sont
packetisés directement en RTP sans passer par des segments de 1 s.

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

Le pipeline (HLS watcher → parser MPEG-TS → H.264 RTP) est partagé avec la
sortie HomeKit ; seul le transport diffère (RTP standard vs SRTP). Le flux
démarre à la demande (première connexion RTSP) et s'arrête quand le dernier
client se déconnecte. Implémenté en Go pur via `bluenviron/gortsplib`, sans
ffmpeg.

## Shutter

Le cache objectif doit être ouvert pour voir l'image :

```
POST http://<camera>:8080/api/shutter
{"open": true}   # ouvrir
{"open": false}  # fermer
```

Le shutter est contrôlé via le canal charmux Shutter (port 8006), pas par fbxhome.

## Limitations

- **Latence ~5s** : inhérente au protocole HLS
- **Pas de HKSV** : HomeKit Secure Video (enregistrement) non implémenté
- **Audio silencieux** : transcodage AAC-ELD en cours (CGo libfdk-aac via zigcc)
- **Shutter + hlcamd conflit** : en mode charmux pur, hlcamd occupe le port Shutter 8007. openqiarad contourne en envoyant via UDP sans bind.
