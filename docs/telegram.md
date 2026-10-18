# Telegram — alertes et commandes à distance

Un bot Telegram prévient le ou les propriétaires quand l'alarme se déclenche
et permet d'agir à distance, **sans aucun matériel ni serveur en plus** : la
caméra parle elle-même à l'API Bot de Telegram. Les connexions sont
uniquement **sortantes** (long polling `getUpdates`) : aucun port à ouvrir
sur la box, la caméra n'est pas exposée sur Internet.

Disponible en mode `standalone` uniquement (en mode `alarmo`, Home
Assistant a ses propres notifications) : avec une config Telegram présente en
mode `alarmo`, le daemon l'ignore et le journalise.

## Ce que ça fait

| Fonction | Détail |
|---|---|
| Alerte au déclenchement | `🚨 Alarme déclenchée — <capteur> — HH:MM:SS` à chaque compte, avec le bouton **🔕 Couper la sirène**, puis une photo, puis un clip vidéo de 15 s. |
| Nouvelle détection pendant le déclenchement | `🚨 Nouvelle détection — <capteur> — HH:MM:SS`, au plus une par capteur et par minute. Un nouveau déclenchement (après un « Couper la sirène ») n'est jamais limité. |
| 🔕 Couper la sirène | Coupe la sirène **sans désarmer** : l'alarme reste armée. Les capteurs encore ouverts au moment de l'appui sont ignorés jusqu'à leur retour au repos ; toute nouvelle détection redéclenche **immédiatement** (sans délai d'entrée) jusqu'au prochain désarmement au clavier. Tous les comptes sont prévenus : `🔕 Sirène coupée par <prénom>. L'alarme reste armée.` |
| 🔒 Armer / 🌙 Armer (nuit) | Armement à distance immédiat (pas de délai de sortie). Si un capteur surveillé est ouvert ou en mouvement, le bot demande confirmation ; « Armer quand même » ignore ces capteurs jusqu'à leur retour au repos. Tous les comptes sont prévenus. |
| ℹ️ État | État de l'alarme (désarmée, armée depuis HH:MM, délai, déclenchée) et capteurs en alerte, ou « Tout est fermé ». |
| 📷 Photo | Photo instantanée, au seul demandeur. |
| 🎥 Vidéo | Clip des 15 s suivantes, au seul demandeur. |
| Plusieurs comptes | Chaque compte autorisé reçoit tout en privé ; un compte injoignable ne retarde jamais les autres. |

**Pas de désarmement depuis Telegram**, par choix de sécurité : un téléphone
volé déverrouillé ou un compte Telegram compromis ne doit pas pouvoir ouvrir
la maison. `/desarmer` répond que le désarmement se fait au clavier.

Les boutons s'affichent en bas de la conversation dès qu'on écrit au bot.
Commandes équivalentes : `/armer`, `/nuit`, `/etat`, `/photo`, `/video`.

## Mise en place

### 1. Créer le bot

Dans Telegram, ouvrir **@BotFather** (coche bleue), envoyer `/newbot`,
choisir un nom et un identifiant (finissant par `bot`). BotFather renvoie un
**jeton** : il donne le contrôle du bot, il ne doit pas circuler.

Conseils :
- Un bot est toujours trouvable par son identifiant : préférer un nom et un
  identifiant **neutres** (pas « alarme » ni le nom de la ville), un
  identifiant aléatoire si possible. La sécurité n'en dépend pas (seuls les
  comptes autorisés sont écoutés), la discrétion si.
- `/setjoingroups` → le bot → **Disable** : le bot ne peut pas être ajouté à
  un groupe.

### 2. Enregistrer son compte

Depuis le dépôt, sur un PC :

```bash
go run ./cmd/telegram-setup -token '<JETON>' -out openqiara_telegram.json
```

L'outil attend un message : ouvrir le bot dans Telegram, appuyer sur
« Démarrer » ou envoyer n'importe quoi. Seule une conversation **privée**
envoyée après le lancement de l'outil est retenue. Le bot répond
`✅ Ce téléphone recevra les alertes.` et le fichier est écrit en `0600`.

### 3. Ajouter un autre compte (facultatif)

Le daemon de la caméra lit les mêmes mises à jour que l'outil : il faut
d'abord **mettre Telegram en pause sur la caméra**, sinon c'est elle qui
recevrait le message du nouveau compte.

```bash
ssh root@<camera> 'mv /data/openqiara_telegram.json /data/openqiara_telegram.json.off; killall openqiarad'
go run ./cmd/telegram-setup -add -out openqiara_telegram.json
```

La nouvelle personne écrit au bot ; l'outil l'ajoute à la liste (le jeton
est relu dans le fichier) et affiche le nombre de comptes.

### 4. Installer la config sur la caméra

```bash
CAM_HOST=<camera> ./scripts/telegram_install.sh openqiara_telegram.json
```

Le script écrit `/data/openqiara_telegram.json` de façon atomique (fichier
temporaire en `0600`, contrôle de taille, `mv`), puis redémarre
`openqiarad`. Dans le log : `telegram notifier started chats=N`. Écrire au
bot doit renvoyer `✅ Alarme connectée.` et afficher les boutons.

## Configuration

`/data/openqiara_telegram.json`, en `0600`, **séparé** de
`/data/openqiara.json` (que le Web UI réécrit en entier) :

```json
{
  "bot_token": "123456789:AA…",
  "chat_ids": [111111111, 222222222]
}
```

- `chat_ids` : conversations privées autorisées (ids positifs). L'ancien
  champ `chat_id` (un seul compte) est encore lu.
- Fichier absent : Telegram désactivé, le reste du daemon fonctionne.
- Fichier invalide : erreur journalisée (sans le jeton), le daemon continue :
  l'alarme locale ne dépend jamais de Telegram.

## Comportement et garanties

- **L'alarme locale ne dépend jamais de Telegram ni d'Internet** : le moteur
  d'alarme ne fait que déposer l'alerte dans une file, sans attendre le réseau.
- **L'alerte texte n'attend jamais le clip** : par compte, texte + bouton,
  puis photo, puis clip quand il est prêt (~20 s plus tard).
- **Réessais** : un texte d'alerte non envoyé (Internet coupé) est réessayé
  avec un backoff de 5 s à 60 s pendant 12 h ; il arrive au retour de la
  connexion avec l'heure réelle de la détection.
- **Autorisation** : une mise à jour n'est traitée que si elle vient d'un
  compte de la liste, en conversation privée (chat = expéditeur). Le reste
  est ignoré et journalisé (ids seulement, jamais le texte).
- **Appuis rejoués** : au démarrage, les mises à jour en attente côté
  Telegram (jusqu'à 24 h) sont purgées sans être exécutées ; un appui sur un
  bouton fait pendant un redémarrage reçoit « Appui ignoré (redémarrage) ».
- **Armer quand même** : la liste des capteurs avertis est portée par le
  bouton ; si un autre capteur s'est ouvert entre l'avertissement et l'appui,
  le bot avertit de nouveau au lieu d'armer.
- **Vidéo** : un seul enregistrement à la fois, partagé par les alertes et
  les demandes simultanées ; MP4 progressif H.264 (flux principal 1080p de
  `hlcamd`), sans son ; taille plafonnée à 8 Mio (`clip.MaxBytes`) pour la
  RAM de la caméra. Au plus un clip d'alerte en cours d'envoi par compte :
  sur une connexion lente, les clips suivants sont sautés, jamais les textes.
- **Volet** : il est ouvert pour la photo et la vidéo, et reste ouvert.
- **Heure** : affichée en heure de Paris (`time/tzdata` embarqué, la caméra
  n'ayant pas de base de fuseaux utilisable par Go). TLS exige une horloge
  juste : la caméra se synchronise seule au démarrage.
- **Racines CA** : `golang.org/x/crypto/x509roots/fallback` est embarqué,
  utilisé si la caméra n'a pas de magasin de certificats.

## Architecture

```
moteur d'alarme ──(hook, non bloquant)──► Notifier.Alert ──► file par compte
                                                              │
       ┌──────────────────────────────────────────────────────┘
       ▼
  goroutine d'envoi (une par compte) : texte+bouton → photo → clip (async)
  goroutine de polling : getUpdates → autorisation → Silence / ArmRemote / État / Photo / Vidéo
```

- `internal/alarm` : `Engine.Silence()`, `Engine.ArmRemote(night, ignoreOpen)`,
  `Engine.InAlarm(night)`, `Engine.SetAlertHook(func(alarm.Alert))`.
- `internal/telegram` : `config.go` (lecture de la config), `bot.go` (client
  Bot API minimal en stdlib), `notifier.go` (files, envois, polling, commandes).
- `internal/clip` : enregistreur de clip MP4 depuis le multicast H.264.
- `cmd/openqiarad/telegram.go` : branchement (volet, photo, clip).
- `cmd/telegram-setup`, `scripts/telegram_install.sh` : mise en place.
