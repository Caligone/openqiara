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

### TLS / mTLS

Pour chiffrer la connexion au broker, utiliser un schéma `ssl://` (ou `tls://`,
`mqtts://`) et le port TLS du broker (typiquement `8883`) :

```json
{
  "mqtt": {
    "broker": "ssl://192.168.1.42:8883",
    "username": "openqiara",
    "password": "openqiara123",
    "tls_ca_cert": "/data/mqtt/ca.pem",
    "tls_client_cert": "/data/mqtt/client.pem",
    "tls_client_key": "/data/mqtt/client.key",
    "tls_insecure": false
  }
}
```

- `tls_ca_cert` : chemin d'un bundle CA (PEM) pour valider le certificat du
  broker. Indispensable pour un broker auto-signé ou à CA privée (Mosquitto
  local). Absent → CA système, qui n'existent probablement pas sur la
  caméra : en pratique, à renseigner.
- `tls_client_cert` / `tls_client_key` : activent le **mTLS** (authentification
  par certificat client). Les deux sont requis ensemble.
- `tls_insecure` : désactive la vérification du certificat. **Test uniquement**,
  jamais en production.

Un fichier de certificat illisible ou invalide fait **échouer** le démarrage du
publisher MQTT (log `ERROR`) au lieu de retomber silencieusement en clair. Les
champs TLS ne sont lus qu'au démarrage : un redémarrage d'openqiarad est requis
pour les appliquer.

**Configuration par fichier uniquement.** Les champs `tls_*` se règlent dans
`openqiara.json` (avec les certificats sur le disque), **pas** via la web UI ni
l'API : le handler `PUT /api/v1/config/mqtt` les ignore, ce qui évite qu'un
formulaire web efface la configuration TLS ou repointe les certificats.

**Cohérence schéma/TLS.** Si un champ `tls_*` est renseigné mais que le broker
n'utilise pas un schéma TLS (`ssl://`, `tls://`, `mqtts://`, `mqtt+ssl://`,
`tcps://`, `wss://`), le publisher MQTT **ne démarre pas** (log `ERROR`, le reste
d'openqiarad tourne) au lieu de se connecter en clair — paho ignorerait sinon la
configuration TLS silencieusement. Pour la même raison, `PUT /api/v1/config/mqtt`
refuse (`400`) un broker sans schéma TLS quand des `tls_*` sont configurés.

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
