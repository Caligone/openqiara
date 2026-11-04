# Appairage et gestion des capteurs

## Vue d'ensemble

| Capteur | Modèle | Persiste MCU |
|---------|--------|--------------|
| DWS (porte) | HOMELABDWS00ACFD | ✅ (0x13) |
| PIR (mouvement) | HOMELABPIR00ACFD | ✅ (0x13) |
| SRN (sirène) | HOMELABSRN00BCFD | ✅ (0x13) |
| KPD (clavier) | HOMELABKPD00ACFD | ✅ (0x13) |

## Appairage

Utiliser la web UI ou l'API :

```
POST http://<camera>/api/v1/sensors/pair
{"type": "DWS"}  # ou PIR, SRN, KPD
```

Le premier capteur du type demandé qui passe en mode appairage est pris. Pas
de QR code ni de fingerprint à saisir. Mettre le capteur en mode appairage :

- DWS, PIR, SRN : retirer et remettre la pile
- KPD : appui long sur le bouton

L'appairage utilise l'opcode 0x13 (mode local/interne) et **persiste dans la NVM du MCU**.

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

Le capteur émet des trames de reinit (`f1 ff 01`) pendant un moment.
Laisser faire quelques minutes avant de conclure à un problème.

Une version antérieure de cette page annonçait ici une limitation
définitive nécessitant un ré-appairage. C'était faux : le RE du
2026-04-22 concluait à partir d'une capture trop courte, et l'erreur a
été corrigée le 2026-05-14 après reproduction en conditions réelles.

Si le capteur reste bloqué en `f1 ff 01` : la clé de session du capteur vit
dans la NVM du MCU et le capteur a perdu sa moitié en perdant
l'alimentation. Il faut alors un factory reset (bouton 10 s) puis un
ré-appairage. Détails dans [`re-findings.md`](re-findings.md) § 6.2.

### PIR — Events mouvement

Même format PKT que DWS. Le type est identifié par le pattern du payload.

### KPD — Events alarme

Format PKT :
- `55 09` = heartbeat
- `55 01 ... 00 01` = arm away (KPD_DAY_ALARM)
- `55 01 ... 00 02` = arm night (KPD_NIGHT_ALARM)
- `55 01 ... 80 00 10 32 00 00` = disarm (KPD_ALARM_OFF, code validé)

**Programmation des codes** : ✅ résolu 2026-05-13 (RE de fbxhome : `endpoints_write ep_name="pwd"`, qui écrivait `<Code valid="true" password="NNNN" />` dans `/data/fbxhome.xml` puis poussait le code au KPD au prochain heartbeat). openqiarad fait de même sans fbxhome : `PUT /api/v1/kpd/codes` enregistre la liste des codes dans sa config, et elle part au prochain réveil du clavier. **Plusieurs codes** : ✅ vérifié 2026-10-29 (deux codes, chacun désarme, un code faux reste refusé). Un clavier sans code n'est pas servi (sa touche OFF seule désarmerait).

### SRN — Sirène

Commandes via l'API : `POST /api/v1/commands/siren/test` (bip de test discret)
et `POST /api/v1/commands/siren/alarm_test` (wail d'intrusion). La durée du
wail part dans la trame radio et la sirène s'arrête d'elle-même.

### SRN — La sirène tient l'alarme

Comme sous fbxhome, c'est la sirène qui tient l'alarme (RE et tests
matériels d'octobre 2026). openqiarad l'arme en lui donnant les délais et
la liste des capteurs surveillés ; elle compte ensuite elle-même le délai de
sortie (bips réguliers), le délai d'entrée (bips d'avertissement) et la
durée de l'alarme.

- **Mode `standalone`** : openqiarad arme la sirène (au clavier avec le
  délai de sortie, depuis HA ou HomeKit sans) et lui signale chaque
  détection (délai d'entrée, ou alerte pour un capteur `instant`). Il
  compte les délais lui aussi, un peu plus longtemps qu'elle : si la
  sirène ne signale pas la fin d'un délai (arrachée, hors de portée),
  l'alarme avance quand même. Une alarme déclenchée ne se quitte que par
  un désarmement.
- **Mode `alarmo`** : Alarmo décide, la sirène suit son état, armée en
  miroir. Réglez les délais d'openqiara comme ceux d'Alarmo.
- Dans les deux modes, openqiarad ramène la sirène à l'état voulu à
  chacun de ses rapports : réarmée si elle a redémarré, désarmée si elle
  est armée à tort. Juste après un démarrage, il la laisse telle quelle
  tant que l'alarme n'a pas dit ce qu'elle veut : elle peut porter une
  alarme survenue pendant la panne.
- **Si la caméra tombe** (panne, Wi-Fi, openqiarad arrêté), les capteurs
  s'adressent directement à la sirène armée, qui déclenche seule.
  Désarmée, elle ne fait rien.

Par capteur, `night_allowed` le retire de la surveillance la nuit et
`instant` le fait déclencher sans délai d'entrée (une fenêtre). Les
délais sont limités à 255 s, l'alarme à 510 s. `siren_sounds: none`
laisse la sirène hors de l'alarme.

### SRN — Recovery après débranchement physique

Si tu débranches puis rebranches physiquement la SRN et qu'elle reste muette
ou injoignable (`reachable=0`, silence radio total > qq minutes) : **débrancher/rebrancher
physiquement** le SRN. Au rebranchement, séquence handshake auto, `reachable` revient à 1.

### Shutter — Contrôle via charmux

Canal Shutter UDP (port 8007 → 8006). Envoi direct sans framing :
- `0x01` = ouvrir
- `0x02` = fermer

(Constat RE : l'API `endpoints_write shutter:true/false` de fbxhome retournait `success:1` sans **effet physique** ; seul le canal charmux agit.)

## Supprimer un capteur

`DELETE /api/v1/sensors/{id}` (voir [`api.md`](api.md)) ou la web UI.

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
