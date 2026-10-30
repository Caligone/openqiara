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
  `/api/v1/commands/alarm`, `/api/codes` est devenu `/api/v1/kpd/codes`).
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

## Authentification

HTTP Basic, utilisateur fixe `admin`, mot de passe défini via
`PUT /api/v1/config/admin` et supprimé via `DELETE /api/v1/config/admin`.
**Tant qu'aucun mot de passe n'est configuré, l'API est ouverte** — y compris
les commandes qui pilotent le matériel.

```console
$ curl -u admin:motdepasse http://camera/api/v1/status
```

L'auth couvre tout le serveur, catch-all `410` compris.

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
- `PUT /kpd/codes` persiste les codes ; ils partent au prochain réveil du
  clavier. Le `200` ne dit rien du clavier, seulement de la config.

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
| `GET` | `/api/v1/sensors` | — | tableau de capteurs + `label`, `night_allowed`, `instant` |
| `PUT` | `/api/v1/sensors/{id}` | au moins un champ parmi `label`, `night_allowed`, `instant` | `{ok:true}` |
| `DELETE` | `/api/v1/sensors/{id}` | — | `{ok:true}` |

`GET /sensors` renvoie l'état connu de chaque capteur, sans interroger la
radio : les capteurs le signalent eux-mêmes, et les changements arrivent
par le flux SSE. `night_allowed` : capteur ignoré quand l'alarme est
armée pour la nuit. `instant` : capteur qui déclenche l'alarme sans
délai d'entrée (une fenêtre). Les deux sont transmis à la sirène, qui
tient l'alarme. `siren_state` (sirène) : l'état qu'elle rapporte.

### Appairage

| Méthode | Chemin | Corps | Réponse |
|---|---|---|---|
| `POST` | `/api/v1/sensors/pair` | `{type}` | `{session}` |
| `GET` | `/api/v1/sensors/pair/{session}` | — | `{done, sensor?}` |
| `DELETE` | `/api/v1/sensors/pair/{session}` | — | `{ok:true}` |

`type` ∈ `DWS`, `PIR`, `SRN`, `KPD` : le premier capteur de ce type qui se
met en mode appairage est retenu. Après 3 appairages ratés d'affilée, la
route répond `500` jusqu'au redémarrage de la caméra (au-delà, le MCU
radio cesse de répondre).

`DELETE /sensors/{id}` arrête de servir le capteur mais ne le retire pas
du MCU, qui n'a aucun moyen d'oublier un capteur : le réinitialiser pour
qu'il se taise.

Boucler sur `GET` jusqu'à `done:true`, puis `DELETE` pour libérer la session
si l'utilisateur abandonne.

### Codes clavier (KPD)

| Méthode | Chemin | Corps | Réponse |
|---|---|---|---|
| `GET` | `/api/v1/kpd/codes` | — | `{sensor_id, codes: [{password, label?}]}` |
| `PUT` | `/api/v1/kpd/codes` | `{codes: [{password, label?}]}` | `{ok:true}` |

`PUT` remplace la liste entière : relire, modifier, renvoyer. Le clavier
reçoit la liste à chaque réveil et vérifie lui-même le code tapé.

- `password` = exactement 4 chiffres, sans doublon dans la liste.
- Au plus 10 codes. Ce n'est pas une limite connue du clavier (deux codes
  vérifiés sur le matériel), mais une marge prudente.
- Une liste vide est acceptée : le clavier n'est alors plus servi, il ne
  peut plus ni armer ni désarmer (sans code, sa touche OFF suffirait à
  désarmer).
- Une liste refusée (`400`) n'est pas enregistrée, même en partie.

Le modèle assume **un seul clavier** :

- `404` — aucun clavier appairé.
- `409` — plusieurs claviers appairés. L'API refuse de choisir plutôt que
  d'écrire sur l'un des deux au hasard.

### Configuration

| Chemin | `GET` renvoie | `PUT` accepte |
|---|---|---|
| `/api/v1/config` | agrégat de tout | — (lecture seule) |
| `/api/v1/config/mqtt` | idem, `password` masqué, `tls_*` en lecture seule | `{broker, username?, password?, topic_prefix?}` |
| `/api/v1/config/homekit` | idem, `pin` masqué | `{enabled, pin?, name?, camera?}` |
| `/api/v1/config/admin` | `{password_set}` — **jamais** le mot de passe ni son hash | `{password}` (8 caractères min., vide refusé) — `DELETE` désactive l'auth |
| `/api/v1/config/alarm` | idem | `{mode?, alarmo_command_topic?, alarmo_state_topic?, siren_sounds?, arming_delay_seconds?, pending_delay_seconds?, wail_duration_seconds?}` |
| `/api/v1/config/web` | `{enabled}` | `{enabled}` |

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
  `none` · délais `0..255 s` (comptés par la sirène) · alarme `1..510 s`.

Notes :

- Les secrets sont masqués en lecture (`********`) : mot de passe MQTT et
  setup code HomeKit. Renvoyer cette valeur telle quelle en écriture laisse
  le secret stocké intact, ce qui permet à un client de relire la config et
  de la réécrire sans effacer ce qu'il n'a pas vu.
- **Les codes du clavier font exception** : `GET /api/v1/kpd/codes` les
  renvoie en clair, parce que l'UI les affiche pour que l'utilisateur les
  retrouve. Ce sont les codes qui désarment l'alarme — ne pas exposer cette
  API hors du réseau local, et configurer un mot de passe admin.
- `PUT /config/mqtt` répond `{"reboot_required":true}` : le broker n'est lu
  qu'au boot. `PUT /config/web` répond `{"restart_required":true}`.

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
| `POST` | `/api/v1/commands/siren/test` | — | son de test discret (sirène désarmée) |
| `POST` | `/api/v1/commands/siren/alarm_test` | — | son de test à pleine puissance, 63 s au plus (sirène désarmée) |
| `POST` | `/api/v1/commands/stream/start` | — | ouvre le cache **et** relance HLS |
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
| `POST` | `/api/v1/commands/debug/siren/sequence` | `{payload, addr, handshake?, stop?, hold_ms?}` |

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

Sert le flux en HLS (MPEG-TS, 1080p + AAC-LC), muxé par openqiarad à la
demande. `HLS_TEST.m3u8`, à la racine ou sous `720p/`, est la playlist
principale. `503` si aucune image n'arrive en 10 s (clapet fermé).

Si `hlcamd` se fige pendant qu'on regarde, openqiarad le relance de lui-même
(plus d'image depuis 10 s) : la lecture reprend après quelques secondes.

