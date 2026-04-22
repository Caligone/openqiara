# MQTT — Intégration Home Assistant

## Configuration

```json
{
  "mqtt": {
    "broker": "tcp://192.168.1.42:1883",
    "username": "openqiara",
    "password": "openqiara123",
    "topic_prefix": "openqiara"
  }
}
```

Si `broker` est vide, MQTT est désactivé et openqiarad tourne sans.

## Auto-discovery HA

OpenQiara publie des configs auto-discovery sur les topics `homeassistant/...` au démarrage.

### DWS (binary_sensor)

```
Topic:   homeassistant/binary_sensor/openqiara_<ID>/config
Payload: {
  "name": "OpenQiara DWS <ID>",
  "unique_id": "openqiara_dws_<ID>",
  "device_class": "opening",
  "state_topic": "openqiara/sensor/<ID>/state",
  "value_template": "{{ value_json.open | lower }}",
  "payload_on": "true",
  "payload_off": "false",
  "device": { "identifiers": ["openqiara_<ID>"], ... }
}
```

State : `{"open": true, "battery": 85, "temperature": 21.5, "reachable": true}`

### PIR (binary_sensor)

```
Topic:   homeassistant/binary_sensor/openqiara_<ID>/config
State:   {"motion": true, "battery": 72, "temperature": 20.0, "reachable": true}
```

### SRN (siren)

```
Topic:     homeassistant/siren/openqiara_<ID>/config
Command:   openqiara/siren/<ID>/set   (payload: "true"/"false" ou "ON"/"OFF")
State:     openqiara/sensor/<ID>/state  {"active": false, "battery": 35, "reachable": true}
```

HA envoie `true`/`ON` → `cam.TriggerSirenAlarm` (test discret en mode
fbxhome, vrai wail en charmux). HA envoie `false`/`OFF` → `cam.StopSiren`
(`reboot_srn` fbxbus en mode fbxhome, 3-5s de resync).

⚠️ En mode `fbxhome`, le wail est limité au son du test discret
(volume bas, ~10s) — voir [`README.md`](../README.md) "Known limitations".

### Alarme (alarm_control_panel)

Publiée en mode `standalone` uniquement (en mode `alarmo`, c'est Alarmo
qui publie sa propre entité `alarm_control_panel`).

```
Topic:     homeassistant/alarm_control_panel/openqiara_alarm/config
State:     openqiara/alarm/state        ("disarmed", "armed_away", "armed_night", "pending", "triggered", "arming")
Command:   openqiara/alarm/set          ("ARM_AWAY", "ARM_NIGHT", "DISARM")
```

Device "OpenQiara Alarm" séparé du KPD physique. Les commandes reçues
sur `openqiara/alarm/set` portent `Source=Remote` côté engine.

### Mode Alarmo bridge

Quand `alarm.mode = "alarmo"` :

- openqiarad subscribe `alarmo/state` (configurable via `alarm.alarmo_state_topic`)
- openqiarad publie sur `alarmo/command` (configurable via `alarm.alarmo_command_topic`)
- Pas d'entité `alarm_control_panel` propre côté openqiarad
- Les transitions `alarmo/state` déclenchent les beeps/wail SRN via `handleSirenForAlarmState`

### Entités supplémentaires

Pour chaque capteur, des entités batterie et température :
```
homeassistant/sensor/openqiara_<ID>_battery/config
homeassistant/sensor/openqiara_<ID>_temperature/config
```

## Sync bidirectionnelle

- **Capteur → HA** : openqiarad publie l'état sur le state topic à chaque event
- **HA → OpenQiara** : openqiarad subscribe aux command topics (alarme, sirène)
- **Web UI → HA** : les changements d'état alarme depuis la web UI sont publiés sur MQTT
- **HA → Web UI** : les commandes HA mettent à jour l'état dans la web UI via le callback
