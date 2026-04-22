# HomeKit — Intégration Apple Home

## Configuration

```json
{
  "homekit": {
    "enabled": true,
    "pin": "00102003",
    "name": "OpenQiara"
  }
}
```

## Entités exposées

| Capteur | Service HomeKit | Caractéristiques |
|---------|----------------|------------------|
| DWS | ContactSensor | ContactSensorState (0=fermé, 1=ouvert) |
| PIR | MotionSensor | MotionDetected (bool) |
| Alarme | SecuritySystem | CurrentState/TargetState — limité via `ValidVals` à 3 modes : `AwayArm`, `NightArm`, `Disarmed`. `AlarmTriggered` apparaît automatiquement quand l'engine passe en `triggered`. Exposé en mode standalone même sans KPD pairé. |
| SRN | Switch | On (bool) — pas de type Siren natif dans HomeKit |
| Shutter | Switch | On (bool) |
| Caméra | IPCamera (H.264 / SRTP) | Flux live via pipeline HLS → MPEG-TS → H.264 RTP → SRTP (pure-Go, sans ffmpeg). Audio silencieux (transcodage AAC CGo à venir) |

## Architecture

Le bridge HomeKit fonctionne en parallèle avec MQTT. Les deux publishers reçoivent les mêmes événements :

```
Capteurs → Core → Publisher MQTT    → HA (MQTT)
                → Publisher HomeKit → Apple Home / HA (HomeKit Controller)
```

## Appairage

1. Activer HomeKit dans la config (`homekit.enabled: true`)
2. Reboot openqiarad
3. Sur iPhone : Maison → Ajouter un accessoire → Code manuel → `001-02-003`
4. Le bridge "OpenQiara" apparaît avec tous les capteurs

HA découvre aussi le bridge via l'intégration HomeKit Controller (mDNS/Bonjour).

## Commandes entrantes

- **SecuritySystem** : quand l'utilisateur arme/désarme depuis Apple Home, le callback `OnAlarmCommand` dispatch via le mode courant (engine local en standalone, `alarmo/command` MQTT en mode Alarmo). `Source=Remote` → pas de délai d'armement, check immédiat post-arm sur les capteurs déjà en alarme.
- **Switch SRN** : `OnSirenCommand` route via `cam.TriggerSirenAlarm` (fbxhome) ou `CharmuxClient.SendSirenAlarm` (charmux). Le "off" déclenche `cam.StopSiren` : en mode fbxhome = `fbxbusctl call fbxhome reboot_srn` (effet de bord : 3-5s de resync SRN avant de pouvoir le retrigger).

## Limitations

- **Audio caméra silencieux** : HomeKit impose AAC-ELD. Le transcodage est en cours (CGo libfdk-aac via zigcc).
- **Pas de HomeKit Secure Video (HKSV)** : enregistrement cloud non implémenté.
- **Pas de type Siren** : HomeKit n'a pas de service Siren natif. La SRN est exposée comme un Switch.
- **Persistance** : le store HomeKit est dans `/data/homekit/`. Si le répertoire est supprimé, il faut re-appairer.

## Streaming caméra

Le flux HomeKit ne passe pas par ffmpeg. Le pipeline est 100% Go :

```
hls (H.264) → /stream/*.ts → HLS watcher → MPEG-TS parser → H.264 RTP → SRTP → iOS
```

Le process `hls` stock est relancé avec `--use-h264` au boot (cf [`../scripts/camera_boot.sh`](../scripts/camera_boot.sh)). Le shutter est ouvert automatiquement à la première demande SRTP.

## Bibliothèque

Utilise `github.com/brutella/hap` (Go pur, ~10-20MB RAM).
