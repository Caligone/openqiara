# Appairage et gestion des capteurs

## Vue d'ensemble

| Capteur | Modèle | Appairage fbxhome | Appairage charmux | QR Code | Persiste MCU |
|---------|--------|----------------|-----------------|---------|--------------|
| DWS (porte) | HOMELABDWS00ACFD | ✅ Direct | ✅ | Non | ✅ (0x13) |
| PIR (mouvement) | HOMELABPIR00ACFD | ✅ Direct | ✅ | Non | ✅ (0x13) |
| SRN (sirène) | HOMELABSRN00BCFD | ✅ Avec QR | ✅ | Oui | ✅ (0x13) |
| KPD (clavier) | HOMELABKPD00ACFD | ✅ Direct | ✅ | Non | ✅ (0x13) |

## Appairage via fbxhome (mode fbxhome)

### DWS / PIR / KPD (pas de QR code)

```bash
SESSION=$(fbxbusctl call fbxhome create_login_session 1 1 | grep -o '"[^"]*"' | tr -d '"')

# Démarrer l'appairage
curl -s -X POST "http://[::1]:10000/api/v1/home/pairing" \
  -d '{"op":"start_adapter","node_type":"HOMELABDWS","adapter_type":"Adapter.DomusAdapter"}' \
  -H "Content-Type: application/json" \
  -H "X-Hlcore-Session-Id: $SESSION"
# Retourne: {"session": <id>, "layout_name": "WaitItem", ...}

# Mettre le capteur en mode appairage :
# - DWS : retirer et remettre la pile
# - PIR : retirer et remettre la pile
# - KPD : appui long sur le bouton

# Poller le résultat
curl -s -X POST "http://[::1]:10000/api/v1/home/pairing" \
  -d '{"op":"poll","session":<id>}' \
  -H "Content-Type: application/json" \
  -H "X-Hlcore-Session-Id: $SESSION"
# layout_name "Terminated" = succès, node_id dans la réponse
```

### SRN (avec QR code)

La SRN nécessite une étape supplémentaire QR code :

1. Démarrer l'appairage (même commande, `node_type: "HOMELABSRN"`)
2. Retirer et remettre les piles de la sirène
3. Poller → `layout_name: "RemoveTab"` (attente piles)
4. Poller → `layout_name: "QRCode"` avec un champ `fingerprint`
5. Envoyer le fingerprint :

```bash
# Le QR code contient des bytes bruts
# Algorithme : hex lowercase des 8 premiers bytes = 16 chars
# Exemple QR raw: 01 23 45 67 89 ab cd ef ... → "0123456789abcdef"

curl -s -X POST "http://[::1]:10000/api/v1/home/pairing" \
  -d '{"op":"next","session":<id>,"fields":["0123456789abcdef"]}' \
  -H "Content-Type: application/json" \
  -H "X-Hlcore-Session-Id: $SESSION"
```

**Attention** : `fields` est une **liste de strings**, pas `[{"value":"..."}]`.

**Piège QR** : `zbarimg` décode en UTF-8, ce qui corrompt les bytes > 0x7F. Utiliser les raw bytes.

## Appairage via charmux (mode charmux)

Utiliser l'API web ou directement `domus.PairSensor()` en Go :

```
POST http://<camera>:8080/api/v1/sensors/pair
{"type": "DWS"}  # ou PIR, SRN, KPD
```

L'appairage charmux utilise l'opcode 0x13 (mode local/interne) et **persiste dans la NVM du MCU**. Pas besoin de QR code.

Séquence radio complète (9 phases) :
1. START_PAIRING (0x13) → MCU ACK
2. Attente beacon du capteur
3. Match vendor key (cofidur1-5)
4. Pair request (UID + vendor key)
5. Challenge reçu
6. Confirmation envoyée
7. Result reçu (adresse radio assignée)
8. Vendor key envoyée sur PKT
9. Appairage complet

## Contrôle des capteurs

### DWS — Events open/close

Format PKT reçu : `01 ADDR F0 xx ADDR ADDR FLAGS 00 01 55 [payload]`
- Payload `00 01` = ouvert
- Payload `40 00` = fermé

#### Après un changement de pile

**En mode `fbxhome` (le mode par défaut), il n'y a rien à faire.** Le
capteur émet des trames de reinit (`f1 ff 01`) pendant un moment, puis
`fbxhome` termine le dialogue tout seul et le DWS revient en ligne.
Laisser faire quelques minutes avant de conclure à un problème.

Une version antérieure de cette page annonçait ici une limitation
définitive nécessitant un ré-appairage. C'était faux : le RE du
2026-04-22 concluait à partir d'une capture trop courte, et l'erreur a
été corrigée le 2026-05-14 après reproduction en conditions réelles.

**En mode `charmux`**, c'est différent : la clé de session du capteur vit
dans la NVM du MCU et le capteur a perdu sa moitié en perdant
l'alimentation. Notre séquence de reinit ne sait pas la rétablir, donc le
capteur reste bloqué en `f1 ff 01`. Il faut alors un factory reset
(bouton 10 s) puis un ré-appairage. Détails dans
[`re-findings.md`](re-findings.md) § 6.2.

### PIR — Events mouvement

Même format PKT que DWS. Le type est identifié par le pattern du payload.

### KPD — Events alarme

Format PKT :
- `55 09` = heartbeat
- `55 01 ... 00 01` = arm away (KPD_DAY_ALARM)
- `55 01 ... 00 02` = arm night (KPD_NIGHT_ALARM)
- `55 01 ... 80 00 10 32 00 00` = disarm (KPD_ALARM_OFF, code validé)

**Programmation des codes** : ✅ résolu 2026-05-13 via `endpoints_write ep_name="pwd"` (HTTP `/api/v1/home/endpoints_write`). fbxhome écrit `<Code valid="true" password="NNNN" />` dans `/data/fbxhome.xml` puis pousse au KPD via bytecode au prochain heartbeat. L'API `add_kpd_pwd` était juste un label d'audit event collectd, pas un endpoint HTTP. Cf. `memory/feedback_kpd_codes.md`. Implémenté côté openqiara via `POST /api/codes`.

### SRN — Contrôle via fbxhome

```bash
# Test sirène (son discret)
curl -sk -X POST "https://[::1]:64218/api/v1/home/endpoints_write" \
  -d '{"list":[{"node_id":<ID>,"endpoints":[{"ep_name":"test","value":true}]}]}' \
  -H "Content-Type: application/json" \
  -H "X-Hlcore-Session-Id: $SESSION"
# Payload radio: 5505010a28 (test_power=10, test_duration=10s)
```

Endpoints SRN exposés : `test`, `alarm_ring`, `sf_test`, `battery`,
`temperature`, `test_power`, `test_duration`.

⚠️ **Limitation découverte 2026-05-14** : `test_power`, `test_duration`
et `alarm_ring` sont **read-only via `endpoints_write`** en mode standalone
(fbxhome répond 200 OK + body `{"message":"Not allowed","reason":5}`).
Conséquence : impossible de pousser un wail full-power via l'API publique.
Le test discret (`test=true`) reste la seule commande utilisable.

### SRN — Recovery après débranchement physique

Si tu débranches puis rebranches physiquement la SRN, deux cas possibles :

1. **SRN `reachable=1` mais muette au test** : `fbxbusctl call fbxhome reboot_srn`. Ça force fbxhome à refaire le handshake complet (`0300...`, `0700...`, `550a16`, `5506`, `550f`). Au prochain `test`, son audible.

2. **SRN `reachable=0`, silence radio total** (> qq minutes sans trafic) : `reboot_srn` ne suffit pas. Il faut **débrancher/rebrancher physiquement** le SRN. Au rebranchement, séquence handshake auto, `reachable` revient à 1.

Strings dans `fbxhome` : `HlSrn::is_data_available() has_rebooted return true` — fbxhome détecte les reboot SRN mais ne re-push pas la config automatiquement.

### Shutter — Contrôle via charmux

Canal Shutter UDP (port 8007 → 8006). Envoi direct sans framing :
- `0x01` = ouvrir
- `0x02` = fermer

En mode fbxhome, l'API `endpoints_write shutter:true/false` retourne `success:1` mais n'a **pas d'effet physique**. Utiliser le canal charmux.

## Supprimer un capteur

```bash
curl -s -X POST "http://[::1]:10000/api/v1/home/delete" \
  -d '{"op":"delete","list":[<node_id>]}' \
  -H "Content-Type: application/json" \
  -H "X-Hlcore-Session-Id: $SESSION"
```

## Dépannage

### Un capteur ne s'appaire pas : commencer par la pile

Avant de conclure au capteur défectueux, **essayer une pile neuve d'une
autre marque**. Une pile faible produit des symptômes trompeurs, observés
et confirmés sur un clavier (2026-05-14) :

- la LED s'allume normalement, les boutons répondent ;
- mais **rien ne part en radio** — l'appairage expire sans que la caméra
  ne voie jamais le capteur.

Le module RF tire un courant bien plus élevé en émission (>100 mA en
crête) que la LED et le MCU réunis. Une pile à 80 % de capacité alimente
encore l'un sans pouvoir alimenter l'autre. Cinq heures de tentatives
avaient été perdues sur ce cas avant le changement de pile, qui a fait
réussir l'appairage en moins de 30 secondes.

Le raisonnement vaut pour tous les capteurs sur pile, même s'il n'a été
mesuré que sur le clavier.
