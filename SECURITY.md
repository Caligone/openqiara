# Politique de sécurité

OpenQiara est un projet **alpha** maintenu par une seule personne sur son
temps libre. Le périmètre de sécurité visé est aujourd'hui **un LAN de
confiance**, pas une exposition Internet.

## Modèle de menace

Ce qui est considéré _hors périmètre_ aujourd'hui :

- Exposition de la caméra sur Internet (port-forwarding, reverse proxy
  public, etc.). **Ne fais pas ça.** Les protections actuelles ne sont
  pas suffisantes pour un attaquant non-LAN.
- Attaque depuis un autre appareil malveillant déjà présent sur le même
  réseau Wi-Fi. La caméra accepte des commandes via MQTT, HomeKit et son
  API HTTP locale ; un attaquant LAN actif peut potentiellement piloter
  l'alarme, redémarrer la caméra, ou (avec le flag `-debug`) envoyer des
  paquets bruts au MCU.

Ce qui est considéré _dans périmètre_ :

- Les credentials du repo (clés vendor, etc.) — il n'y en a aucun par
  conception.
- L'intégrité du firmware constructeur sous-jacent — `openqiarad` est un
  overlay, il ne re-flashe pas le firmware MCU.
- La récupération en cas de bricolage utilisateur (backups SD documentés
  dans le README).

## Limitations sécurité connues (à corriger)

Toutes documentées dans [`README.md`](README.md) § "Limitations connues",
listées ici pour référence :

- Mot de passe admin stocké en clair dans `/data/openqiara.json`
- HTTP Basic Auth sans comparaison constant-time → timing leak théorique
- CORS `Access-Control-Allow-Origin: *` global, y compris sur les routes
  mutantes (`POST/PUT/DELETE`)
- Pas de protection CSRF, pas de rate limiting sur l'auth
- Web UI : certains points d'injection HTML non échappés systématiquement
- `/events` (push fbxhome → openqiarad) accessible sans auth
- Endpoints `/api/debug/pkt` et `/api/debug/siren/sequence` activables
  via `-debug` — peuvent brick le MCU (opcodes `0x03`, `0x08`)
- Le boot script ouvre tous les ports en INPUT (politique ACCEPT all)

Le durcissement est planifié mais ne doit pas être considéré comme acquis.

## Signaler une vulnérabilité

Pour les questions de sécurité sensibles, préférer un canal privé :

1. **Issue GitHub privée** : ouvrir une advisory via
   [GitHub Security Advisories](https://github.com/Caligone/openqiara/security/advisories/new).
2. **Sinon, issue publique** sur https://github.com/Caligone/openqiara/issues
   en commençant le titre par `[security]`.

Les vulnérabilités triviales (cf. liste "Limitations sécurité connues"
ci-dessus) sont déjà connues — pas la peine de les signaler. En revanche,
**tout chemin d'exploitation distant non documenté** (ex : RCE via une
requête HTTP, exploit non-LAN, exfiltration de credentials) mérite un
report rapide.

## Pas de support stable

Tant que le projet est en alpha, il n'y a pas d'engagement de réponse
sous délai, ni de release de sécurité tagguée. Les correctifs sont
poussés directement sur `master`.
