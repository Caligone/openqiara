# API HTTP

`openqiarad` expose une API REST sur le port du serveur web (`-web`, `:80`
par défaut). C'est la seule interface programmatique : l'UI embarquée ne
fait rien qu'un client tiers ne puisse faire.

Toutes les réponses sont en JSON, y compris les erreurs :

```json
{"error": "le code doit être composé de 4 chiffres"}
```

> **Spécification machine** — [`openapi.json`](openapi.json) (OpenAPI 3.1)
> décrit les mêmes routes avec leurs schémas complets : de quoi générer un
> client, alimenter Postman ou Bruno, ou produire une page de documentation :
>
> ```console
> $ npx @redocly/cli@latest build-docs docs/openapi.json -o /tmp/api.html
> ```
>
> Cette page-ci reste la lecture de départ : elle déroule les cas d'usage dans
> l'ordre où on les rencontre, là où la spécification est organisée par route.

## Versionnage

Tout est sous **`/api/v1`**. Deux réponses distinguent une route disparue
d'une route inexistante :

- **`410 Gone`** — une route non versionnée, retirée définitivement. Les
  correspondances sont dans les tableaux ci-dessous ; elles ne sont pas
  calculables mécaniquement (`/api/alarm` est devenu
  `/api/v1/commands/alarm`, `/api/codes` est devenu `/api/v1/kpd/code`).
- **`404 Not Found`** — une route `/api/v1/…` absente de cette version :
  faute de frappe, méthode non supportée, ou route plus récente que le
  daemon installé. Réessayer après mise à jour a du sens, là où un `410`
  est définitif.

```console
$ curl -s http://camera/api/status
{"error":"API non versionnée supprimée, voir /api/v1","path":"/api/status","see":"docs/api.md","version":"v1"}

$ curl -s http://camera/api/v1/typo
{"error":"route inconnue","path":"/api/v1/typo","method":"GET","see":"docs/api.md"}
```

Deux exceptions volontaires, **hors versionnage et hors contrat** :
`POST /events` et `POST /notifications` (cf. [Webhooks internes](#webhooks-internes)).

## Authentification

HTTP Basic, utilisateur fixe `admin`, mot de passe défini via
`PUT /api/v1/config/admin`. **Tant qu'aucun mot de passe n'est configuré,
l'API est ouverte** — y compris les commandes qui pilotent le matériel.

```console
$ curl -u admin:motdepasse http://camera/api/v1/status
```

L'auth couvre tout le serveur, catch-all `410` compris. Seuls les webhooks
loopback y échappent (et uniquement depuis `127.0.0.1`/`::1`).

## Ressources et commandes

L'API distingue deux familles, et la distinction n'est pas cosmétique.

| | Ressources | Commandes |
|---|---|---|
| Chemins | `/sensors`, `/config`, `/alarm`, `/kpd`, `/status`, `/update` | `/commands/*` |
| Sémantique | un état qu'on lit et qu'on écrit | un effet matériel qu'on déclenche |
| `GET` sûr, `PUT` idempotent | oui | sans objet |
| Ce que signifie `200` | l'état est celui décrit | la commande est **acceptée** |

**Une commande qui répond `200` n'a pas forcément été exécutée.** Elle a été
acceptée. Trois cas où l'écart est visible :

- `POST /commands/reboot` répond **avant** de redémarrer. Un échec de
  `reboot` n'est jamais remonté.
- `POST /commands/alarm` renvoie un état *optimiste*. En mode `alarmo`, la
  vérité arrive quelques centaines de ms plus tard via MQTT et peut
  contredire la réponse. Pour l'état réel, lire `GET /api/v1/alarm` ou
  s'abonner au flux SSE.
- `PUT /kpd/code` persiste le code immédiatement mais **diffère l'écriture
  radio jusqu'à 30 s** si le clavier vient d'être appairé (le cycle bytecode
  post-pairing ne doit pas être interrompu). Le `200` ne dit rien du
  clavier, seulement de la config.

Ne « corrigez » pas les commandes en ressources REST : elles ne modélisent
pas un état, elles déclenchent un effet.

---

## Ressources

### État

| Méthode | Chemin | Réponse |
|---|---|---|
| `GET` | `/api/v1/status` | `{uptime, mqtt_connected, sensor_count, version}` |
| `GET` | `/api/v1/alarm` | `{state, armed_at?, triggered_by?, previous_state?, timer_remaining?}` |

`state` ∈ `disarmed`, `armed_away`, `armed_night`, `arming`, `pending`,
`triggered`.

### Capteurs

| Méthode | Chemin | Corps | Réponse |
|---|---|---|---|
| `GET` | `/api/v1/sensors` | — | tableau de capteurs + `label`, `night_allowed`, `day_alarm?`, `night_alarm?`, `day_timed?`, `night_timed?` |
| `PUT` | `/api/v1/sensors/{id}` | au moins un champ parmi `label`, `night_allowed`, `day_alarm`, `night_alarm`, `day_timed`, `night_timed` | `{ok:true}` |
| `DELETE` | `/api/v1/sensors/{id}` | — | `{ok:true}` |

`GET /sensors` ne lit pas l'état temps réel des capteurs (batterie,
température, ouverture) : chaque lecture attendrait le MCU ~3 s, soit 15 s
pour cinq capteurs. Ces valeurs arrivent par le flux SSE.

`PUT /sensors/{id}` fait plus que renommer : en mode fbxhome, chaque flag
modifié déclenche un `endpoints_write` vers le firmware. **Ces écritures
sont best-effort** — un échec est loggué en warning, pas remonté dans la
réponse. Le `200` couvre la persistance en config, pas le push firmware.

### Appairage

| Méthode | Chemin | Corps | Réponse |
|---|---|---|---|
| `POST` | `/api/v1/sensors/pair` | `{type, fingerprint}` | `{session}` |
| `GET` | `/api/v1/sensors/pair/{session}` | — | `{done, sensor?}` |
| `DELETE` | `/api/v1/sensors/pair/{session}` | — | `{ok:true}` |

`type` ∈ `DWS`, `PIR`, `SRN`, `KPD`. `fingerprint` = 16 caractères hex du QR
code ; les tirets, espaces et majuscules sont normalisés, une chaîne de
32 caractères est tronquée aux 16 premiers.

Boucler sur `GET` jusqu'à `done:true`, puis `DELETE` pour libérer la session
si l'utilisateur abandonne.

### Code clavier (KPD)

| Méthode | Chemin | Corps | Réponse |
|---|---|---|---|
| `GET` | `/api/v1/kpd/code` | — | `{sensor_id, code: {password, label} \| null}` |
| `PUT` | `/api/v1/kpd/code` | `{password, label?}` | `{ok:true}` |
| `DELETE` | `/api/v1/kpd/code` | — | `{ok:true}` |

`password` = exactement 4 chiffres. Un seul code par installation : c'est
une contrainte du firmware, pas un choix d'API.

Le modèle assume **un seul clavier** :

- `404` — aucun clavier appairé.
- `409` — plusieurs claviers appairés. L'API refuse de choisir plutôt que
  d'écrire sur l'un des deux au hasard.

### Configuration

| Chemin | `GET` renvoie | `PUT` accepte |
|---|---|---|
| `/api/v1/config` | agrégat de tout + `camera_mode` | — (lecture seule) |
| `/api/v1/config/mqtt` | idem, `password` masqué, `tls_*` en lecture seule | `{broker, username?, password?, topic_prefix?}` |
| `/api/v1/config/homekit` | idem, `pin` masqué | `{enabled, pin?, name?, camera?}` |
| `/api/v1/config/admin` | `{password_set}` — **jamais** le mot de passe ni son hash | `{password}` |
| `/api/v1/config/alarm` | idem | `{mode?, alarmo_command_topic?, alarmo_state_topic?, siren_sounds?, arming_delay_seconds?, pending_delay_seconds?, wail_duration_seconds?}` |
| `/api/v1/config/web` | `{enabled}` | `{enabled}` |
| `/api/v1/config/fbxhome_alarm` | idem | `{timeout_before_armed, timeout_before_alert, timeout_alert, history_when_armed}` |

Contraintes validées côté serveur (`400` sinon) :

- `broker` — schéma parmi `tcp`, `ssl`, `tls`, `mqtt`, `mqtts`, `ws`, `wss`
  (ou aucun), hôte non vide, port numérique dans `1..65535`. Schéma TLS
  obligatoire si des `tls_*` sont configurés (fichier uniquement, cf.
  [mqtt.md](mqtt.md)).
- `pin` HomeKit — 8 chiffres, codes triviaux refusés (Apple les rejette de
  toute façon à l'appairage).
- `password` admin — 8 caractères minimum, ou chaîne vide pour **désactiver
  l'authentification**.
- `mode` ∈ `standalone`, `alarmo` · `siren_sounds` ∈ `all`, `alarm_only`,
  `none` · délais `0..600 s` · wail `1..60 s`.

Notes :

- Les secrets sont masqués en lecture (`********`) : mot de passe MQTT et
  setup code HomeKit. Renvoyer cette valeur telle quelle en écriture laisse
  le secret stocké intact, ce qui permet à un client de relire la config et
  de la réécrire sans effacer ce qu'il n'a pas vu.
- **Le code PIN du clavier fait exception** : `GET /api/v1/kpd/code` le
  renvoie en clair, parce que l'UI l'affiche pour que l'utilisateur le
  retrouve. C'est le code qui désarme l'alarme — ne pas exposer cette API
  hors du réseau local, et configurer un mot de passe admin.
- `PUT /config/mqtt` répond `{"reboot_required":true}` : le broker n'est lu
  qu'au boot. `PUT /config/web` répond `{"restart_required":true}`.
- `camera_mode` (`fbxhome` ou `charmux`) indique à l'UI quels réglages ont
  un sens. `/config/fbxhome_alarm` répond `400` en mode charmux.

### Mise à jour

| Méthode | Chemin | Réponse |
|---|---|---|
| `GET` | `/api/v1/update` | `{current, latest, update_needed, checked_at}` |
| `GET` | `/api/v1/update/status` | `{step, version?, progress?, bytes?, total_bytes?, error?}` |

`503` si l'OTA n'est pas configuré (build local sans `-ldflags`), `502` si
GitHub est injoignable.

---

## Commandes

| Méthode | Chemin | Corps | Effet |
|---|---|---|---|
| `POST` | `/api/v1/commands/alarm` | `{action}` | `arm_away`, `arm_night` ou `disarm` |
| `POST` | `/api/v1/commands/reboot` | — | redémarre la caméra |
| `POST` | `/api/v1/commands/shutter` | `{open}` | ouvre/ferme le cache objectif |
| `POST` | `/api/v1/commands/siren/test` | — | bip de test discret |
| `POST` | `/api/v1/commands/siren/alarm_test` | — | wail d'intrusion |
| `POST` | `/api/v1/commands/stream/start` | — | ouvre le cache **et** relance HLS |
| `POST` | `/api/v1/commands/stream/open` | — | `{srt_url, passphrase, port}` |
| `POST` | `/api/v1/commands/update/install` | `{tag?}` | `202 Accepted`, suivre `/update/status` |

`404` si aucune sirène n'est appairée. `409` si une installation OTA est
déjà en cours.

`stream/start` ouvre le cache *et* relance le pipeline : un échec du cache
est loggué sans être fatal, un échec du pipeline renvoie `500`.

### Commandes de debug

Montées uniquement avec le flag `-debug` ; sinon `410`.

| Méthode | Chemin | Corps |
|---|---|---|
| `POST` | `/api/v1/commands/debug/pkt` | `{hex}` |
| `POST` | `/api/v1/commands/debug/siren/sequence` | `{payload, addr?, handshake?, stop?, hold_ms?}` |

> **Ces endpoints envoient des trames brutes au MCU.** Certains opcodes
> (`0x03`, `0x08`) le font planter, et une remise en service demande un
> rootfs stock. Hors contrat, aucune validation métier, à n'activer qu'en
> développement.

---

## Flux d'événements (SSE)

```
GET /api/v1/events
```

Flux `text/event-stream`. Quatre types :

| Type | Charge utile |
|---|---|
| `status` | `{mqtt_connected, sensor_count, uptime, version}`, toutes les 5 s |
| `alarm` | même forme que `GET /api/v1/alarm` |
| `sensor` | un capteur, poussé à chaque event radio |
| `sensors` | nombre de capteurs, après une mutation de la liste |

**À la connexion, le dernier événement de chaque type est rejoué** — un
client qui se (re)connecte obtient immédiatement l'état courant sans
attendre le prochain push ni faire de `GET` de rattrapage.

Un commentaire `: connected` est envoyé à l'ouverture, puis un ping toutes
les 30 s pour tenir la connexion à travers les proxies.

```console
$ curl -N http://camera/api/v1/events
: connected

event: status
data: {"mqtt_connected":false,"sensor_count":3,"uptime":45,"version":"dev"}

event: alarm
data: {"state":"armed_away","armed_at":1789891779}
```

---

## Flux vidéo

```
GET /stream/{fichier}
```

Sert les artefacts HLS produits par `hlcamd` depuis `/tmp/out_stream/stream/`.
Extensions acceptées : `.m3u8`, `.m4s`, `.ts` — tout le reste est `404`.
Les `.m4s` contiennent du MPEG-TS malgré l'extension.

Une requête sur la playlist déclenche un *lazy healing* si elle n'a pas été
écrite depuis trop longtemps : la requête courante peut échouer, la suivante
sera servie.

---

## Webhooks internes

```
POST /events
POST /notifications
```

**Hors contrat, hors versionnage, ne pas appeler.** Ces routes existent pour
`hl_event_collectd`, le collecteur vendor qui croit pousser vers le cloud
Free : le DNS local résout `*.srv.home-labs.fr` vers `127.0.0.1`.

Trois propriétés à ne pas casser :

- **Loopback strict** — refusées en `403` depuis toute autre adresse, y
  compris quand l'authentification est désactivée.
- **Toujours `200`** — même sur un corps illisible. Un autre code met le
  collecteur en file de retry et **bloque tous les events suivants**.
- **`Server: nginx/1.14.2` et `{"result":"ok"}`** — la réponse imite le
  cloud Free. Ce header n'est pas un résidu : le retirer fait échouer la
  livraison.

Ce sont ces routes qui alimentent les events DWS, PIR, KPD et la détection
IntelliVision.
