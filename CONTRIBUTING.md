# Contribuer

Merci de l'intérêt. Ce document couvre ce que le code ne dit pas.

Le projet est maintenu par une seule personne sur son temps libre : une PR
peut mettre quelques jours à être relue. Pour une question, une idée ou un
doute avant de coder, ouvrez une
[issue](https://github.com/Caligone/openqiara/issues) — c'est le seul canal,
hors vulnérabilités (voir [`SECURITY.md`](SECURITY.md)).

En contribuant, vous acceptez que votre code soit distribué sous la licence
du projet, [MIT](LICENSE).

## Avant d'ouvrir une PR

```bash
make test    # go test -race -count=1 ./...
make lint    # golangci-lint run ./...
make daemon  # cross-compile ARMv7
```

La CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) lance
l'équivalent sur Go 1.26 avec golangci-lint v2.12.2 — une version locale
différente peut donner un autre résultat. Le `-count=1` désactive le cache de
test : un test vert en local depuis le cache peut échouer en CI.

Le binaire tourne sur ARMv7 : une dépendance qui ne cross-compile pas est
inutilisable, autant s'en apercevoir tout de suite.

Les PR ciblent `master` — la CI ne se déclenche que sur cette base.

## Commits

Le dépôt suit [Conventional Commits](https://www.conventionalcommits.org/fr/)
et le changelog de release est généré depuis les sujets de commit
([`release.yml`](.github/workflows/release.yml)) : un sujet non conforme
atterrit tel quel dans les notes publiques.

```
fix(pir): remove misleading cover state
feat(rtsp): native RTSP server sharing one HLS pipeline with HomeKit
refactor(api): move the HTTP API under /api/v1
docs: add an OpenAPI description of the HTTP API
```

Portées vues dans l'historique : `api`, `web`, `sensors`, `pir`, `alarm`,
`mqtt`, `ota`, `sd`, `rtsp`, `release`. Les sujets sont en anglais.

## Toucher à l'API : trois fichiers, pas un

L'API vit dans `internal/web/server.go`, mais elle est décrite à deux autres
endroits. **Ajouter, renommer ou supprimer une route, changer un champ de
requête ou de réponse, un code HTTP ou une validation, c'est mettre à jour
les trois dans le même commit :**

| Fichier | Rôle |
|---|---|
| `internal/web/server.go` | la vérité : routes, handlers, validations |
| `docs/openapi.json` | spécification OpenAPI 3.1 — schémas, codes, exemples |
| `docs/api.md` | référence lisible — explique le *pourquoi* |

Rien n'automatise cette synchronisation : pas de génération depuis le code,
pas de test de parité. **Une spec qui ment est pire que pas de spec** — un
intégrateur lui fait confiance et découvre l'écart en production.

Après une modification, valider la grammaire de la spec :

```bash
npx @redocly/cli@latest lint docs/openapi.json   # doit finir sans erreur
```

`lint` ne lit jamais `server.go` : il ne verra **ni** une route oubliée dans
la spec, **ni** une route inventée qui n'existe pas. La parité se vérifie à la
main :

```bash
grep -o 'HandleFunc("[A-Z]* /api/v1[^"]*"' internal/web/server.go | sort
```

Restent hors spec, volontairement :

- `POST /events` et `POST /notifications` — webhooks du daemon constructeur,
  sans contrat ;
- `OPTIONS /api/` — préflight CORS ;
- les filets `404` (route `/api/v1/` inconnue) et `410` (route non
  versionnée) — leur comportement est décrit dans `info.description` de la
  spec, pas comme des opérations ;
- `GET /` — l'interface web embarquée.

Les principes que la spec encode (ressources contre commandes, séparation
lecture/écriture des secrets) sont expliqués dans `info.description` de
`docs/openapi.json`. Les lire avant de modifier un schéma, plutôt que de les
redécouvrir.

## Tests

Les tests tournent sur une machine de développement, sans caméra.

Mocker le **transport** est légitime et déjà fait : `internal/charmux` monte
un faux endpoint UDP, `internal/camera` un `fbxbusctl` factice. Ce qui ne se
mocke pas, c'est la **sémantique du capteur** — inventer une réponse à une
trame jamais observée ne prouve rien, et le protocole DomusRF a trop de
comportements non documentés pour qu'un tel mock reste fidèle. Un test sur
une trame part d'une trame réellement capturée.

Une modification qui touche le pilotage matériel se valide sur une vraie
caméra. Dites-le dans la PR, avec ce que vous avez observé.

## Documentation

- `README.md` et la majorité de `docs/` : **en français**, la base
  d'utilisateurs l'est.
- `docs/protocol.md`, `docs/re-findings.md`, `docs/kpd.md` : **en anglais**,
  c'est de la rétro-ingénierie pure et les contributeurs potentiels sont
  internationaux.
- Commentaires de code : suivez le fichier.

## Rétro-ingénierie

Le protocole DomusRF a été reconstruit par observation. Si vous documentez un
comportement :

- **citez la trame hexadécimale et la ligne de log** qui l'établissent ;
- si c'est une hypothèse non vérifiée sur le matériel, **dites-le
  explicitement**. Une hypothèse présentée comme un fait coûte des heures à
  celui qui la reprendra.

Une chaîne de caractères dans le binaire constructeur ne prouve pas que le
capteur émet la donnée. `fbxhome` contient une chaîne `Pir tamper` et son
parseur accepte une valeur `2`, mais aucun PIR observé ne l'a jamais émise :
une implémentation avait été construite sur cette base, puis entièrement
retirée ([#30](https://github.com/Caligone/openqiara/issues/30)).

## Sécurité

Une vulnérabilité se signale en privé : voir [`SECURITY.md`](SECURITY.md).

Les endpoints `/api/v1/commands/debug/*` envoient des trames brutes au MCU et
peuvent le rendre inutilisable. Ils ne sont montés qu'avec `-debug` — ne
jamais les activer par défaut, ni supprimer ce garde-fou.
