# Premiers pas avec OpenQiara

Ce guide t'accompagne d'une caméra Qiara stock (ou défunte) à un setup
OpenQiara fonctionnel qui parle à Home Assistant. Il suppose que tu es à
l'aise avec SSH et Home Assistant, mais **ne suppose pas** de connaissances
en reverse engineering.

> **TL;DR** — OpenQiara est un daemon overlay qui remplace l'application
> `fbxhome` cloud-dépendante d'une caméra Qiara par un service Go 100% local.
> Tes capteurs continuent de marcher, ta caméra n'a plus besoin des serveurs
> Qiara (désormais défunts), et tout se retrouve dans MQTT.

---

## Ce qu'il te faut

**Matériel :**
- Une caméra Qiara (le modèle rond avec un cache vie privée — modèle "HOMELABCAM")
- Ses capteurs 868 MHz existants (DWS, PIR, KPD, SRN). OpenQiara ne peut pas
  appairer des capteurs neufs achetés ailleurs.
- Un lecteur de microSD sur ton ordinateur (pour accéder à la SD de la caméra
  si tu as besoin de récupération offline — pas requis pour le happy path)

**Sur ton ordinateur :**
- Linux ou macOS avec Go 1.26+ installé
- Une paire de clés SSH (`~/.ssh/id_ed25519` fait l'affaire)

**Sur ton réseau :**
- Un réseau WiFi 2.4 GHz (la puce WiFi de la caméra est 2.4 GHz uniquement)
- Un broker MQTT joignable depuis la caméra (Mosquitto fait l'affaire)
- Une instance Home Assistant avec l'intégration MQTT activée

---

## Ce dont tu n'as PAS besoin

- Un compte cloud Qiara (le cloud est mort de toute façon)
- Une connexion internet sur la caméra (tout est local)
- Un firmware re-flashé — OpenQiara tourne **au-dessus** du rootfs stock

---

## Étape 1 — Accès SSH à la caméra

Si ta caméra est encore en ligne depuis l'époque Qiara, elle est probablement
déjà connectée à ton WiFi. Trouve son IP :

```bash
arp -a | grep -i lwip
```

Tu devrais voir une entrée, du genre `192.168.1.50`.

Si ta caméra a été factory-resettée ou jamais provisionnée, il faudra préparer
la SD offline d'abord — voir [`install.md`](install.md) pour la procédure de
préparation SD. Reviens ici une fois que tu as l'accès SSH.

Teste la connexion :

```bash
ssh root@192.168.1.50
```

Le rootfs stock de la caméra autorise le login root via SSH sans mot de passe.
**Ajoute ta clé publique à `/root/.ssh/authorized_keys`** dès que tu es dedans
— tu te remercieras plus tard.

---

## Étape 2 — Build OpenQiara sur ton ordinateur

Clone le repo et cross-compile pour ARM :

```bash
git clone https://github.com/Caligone/openqiara
cd openqiara
GOOS=linux GOARCH=arm GOARM=7 go build -trimpath -ldflags="-s -w" \
    -o bin/openqiarad ./cmd/openqiarad
```

Ça produit un binaire unique lié statiquement à `bin/openqiarad` (~10 MB).
Pas besoin de runtime Go sur la caméra.

---

## Étape 3 — Configurer et déployer

Le repo fournit deux scripts qui automatisent le déploiement. Mets l'IP de la
caméra et lance-les :

```bash
export CAM_HOST=192.168.1.50          # IP de ta caméra
export SSH_KEY=~/.ssh/id_ed25519       # ta clé SSH
export MQTT_BROKER=tcp://192.168.1.10:1883
export MQTT_USER=openqiara
export MQTT_PASS=changeme

./scripts/configure.sh   # écrit /data/openqiara.json sur la caméra
./scripts/deploy.sh      # cross-compile, upload, redémarre
```

Tu devrais voir `openqiarad` démarrer dans le log tailé à la fin de
`deploy.sh`. Si tu vois `MQTT connected` et `charmux ready`, c'est bon.

---

## Étape 4 — Le faire survivre à un redémarrage

Par défaut, `deploy.sh` lance `openqiarad` uniquement pour le boot courant.
Pour le faire auto-démarrer, installe le boot hook :

```bash
./scripts/install_boot.sh
```

Ça patche `/etc/init.d/rcS.real` pour lancer `/data/boot.sh` à chaque boot,
qui à son tour démarre `openqiarad`.

Reboote pour vérifier :

```bash
ssh root@$CAM_HOST reboot
# Attendre ~60 secondes
ssh root@$CAM_HOST 'pidof openqiarad && echo OK'
```

---

## Étape 5 — Ouvrir la web UI

OpenQiara sert une petite web UI à `http://openqiara.local` (port 80).
Depuis là tu peux :

- **Voir l'état des capteurs** en temps réel (motion, porte ouverte/fermée, codes KPD)
- **Appairer de nouveaux capteurs** (clic "Pairing" → saisir le fingerprint QR)
- **Configurer le moteur d'alarme** (mode standalone ou bridge Alarmo)
- **Régler les timings** (délai d'armement, délai pending, durée du wail)
- **Définir le PIN du clavier**
- **Déclencher la sirène manuellement** pour tester

À la première ouverture, on te demandera de définir un mot de passe admin.

---

## Étape 6 — Appairer ton premier capteur

1. Dans la web UI, clic **Capteurs → Appairer**
2. Choisis le type de capteur (DWS / PIR / KPD / SRN)
3. Appui long sur le **bouton d'appairage** du capteur pendant 3 secondes (il
   est sous le couvercle de la pile pour la plupart des types de capteurs)
4. Attends jusqu'à 30 secondes — l'UI montrera le nouveau capteur dès que le
   handshake est terminé
5. Le capteur est maintenant appairé. Ouvre MQTT Explorer (ou Home Assistant)
   et tu devrais voir une nouvelle entité sous le préfixe de topic `openqiara/`

Si l'appairage échoue : voir [`docs/sensors.md`](sensors.md) pour le
troubleshooting spécifique au capteur et l'emplacement du bouton d'appairage
par type.

---

## Étape 7 — Câbler à Home Assistant

OpenQiara publie les messages **MQTT discovery** Home Assistant, donc les
capteurs devraient apparaître automatiquement dans HA sous **Settings →
Devices & Services → MQTT → Devices**.

Si tu ne les vois pas :

1. Vérifie que l'intégration MQTT de HA utilise le même broker que `openqiara.json`
2. Vérifie que le préfixe de topic est bien `homeassistant/` par défaut pour la discovery
3. Vérifie que le préfixe `openqiara/` apparaît dans MQTT Explorer

### Standalone vs Alarmo

OpenQiara propose **deux modes d'alarme** :

- **Standalone** (par défaut) : la machine à états d'alarme locale dans
  `openqiarad` possède l'état. L'armement via KPD applique un délai d'armement
  (10s par défaut, configurable). L'armement via HK/HA est instantané. Les
  capteurs en alarme au moment de l'armement déclenchent immédiatement en mode
  Remote, avec une période de grâce en mode Local.

- **Bridge Alarmo** : le composant
  [Alarmo](https://github.com/nielsfaber/alarmo) de Home Assistant est la
  source de vérité. `openqiarad` transmet les commandes utilisateur à
  `alarmo/command` et reflète `alarmo/state` vers MQTT/HK. Utile si tu
  centralises déjà la logique d'alarme dans HA.

Bascule via l'onglet Configuration de la web UI ; un redémarrage du daemon
est nécessaire pour que le changement prenne pleinement effet. Voir
[`docs/mqtt.md`](mqtt.md) pour la référence complète des topics.

---

## Problèmes courants

**"Capteurs appairés mais pas d'events"**
Le canal PKT a besoin d'un ACK initial de la gateway pour commencer à
forwarder. Ça se passe automatiquement au démarrage d'openqiarad ; si ça
échoue, regarde le log pour `pkt forwarding enabled`. Un reboot règle
généralement le souci.

**"Le KPD montre le mauvais PIN"**
Le clavier ne supporte qu'**un seul** PIN à la fois, et ce PIN a été gravé
pendant le setup Qiara d'origine. Pour le changer : appaire le clavier à neuf
depuis OpenQiara et définis un nouveau PIN via la web UI. Voir
[`docs/kpd.md`](kpd.md).

**"Batterie / température affiche 0"**
Batterie et température en mode charmux ne sont pas lisibles actuellement —
fbxhome obtenait ces valeurs du cloud Qiara Sigfox qui est offline. La web UI
masque les champs à 0. Voir la section "Open questions" de
[`docs/protocol.md`](protocol.md) pour l'état actuel du RE.

**"La caméra redémarre aléatoirement"**
Vérifie que tu n'envoies pas d'opcodes CTRL dangereux (`0x03`, `0x08`) — ils
crashent le MCU et corrompent la NVM. OpenQiara n'envoie jamais ces opcodes
par design.

---

## Prochaines étapes

- [`docs/sensors.md`](sensors.md) — appairage, events, et particularités par type
- [`docs/mqtt.md`](mqtt.md) — référence des topics MQTT et discovery HA
- [`docs/protocol.md`](protocol.md) — référence protocole DomusRF (pour contributeurs)
- [`docs/architecture.md`](architecture.md) — architecture interne de `openqiarad`
- [`docs/install.md`](install.md) — installation avancée (prép SD, recovery)

---

## Disclaimer

OpenQiara est un projet d'interopérabilité. Il tourne au-dessus du rootfs
stock Qiara/Free et utilise des binaires constructeur (`charmux`, `hlcamd`,
`uartboot`, modules kernel) qui restent sur l'appareil inchangés. **Utilisation
à tes risques et périls.** Modifier ta caméra peut annuler la garantie
constructeur (mais Qiara étant défunte, c'est surtout théorique) et peut
briquer l'appareil si tu ignores les avertissements sur les opcodes MCU
dangereux.

Fais un backup de `/dev/mmcblk0p1` (la partition rootfs) avant tout truc
irréversible :

```bash
ssh root@$CAM_HOST 'dd if=/dev/mmcblk0p1 | gzip' > rootfs-backup.img.gz
```

Garde ce fichier en lieu sûr. C'est ton billet retour si quelque chose tourne mal.
