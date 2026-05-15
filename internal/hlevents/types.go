// Package hlevents parse les notifications cloud poussées par
// `hl_event_collectd` sur les endpoints HTTP `/events` et `/notifications`.
//
// hl_event_collectd est un daemon vendor (Free/Iliad) qui agrège tous les
// events maison-connectée (alarme, caméra, capteurs, IntelliVision) et les
// pousse vers le cloud Qiara via HTTP POST. Le cloud étant mort, on
// intercepte ces POST en ré-aiguillant le DNS local `srv.home-labs.fr` →
// `127.0.0.1` (cf. scripts/camera_boot.sh).
//
// Les payloads contiennent une queue d'events horodatés. Cf. les types
// EventEnvelope / NotificationEnvelope ci-dessous pour la structure.
package hlevents

import "time"

// EventEnvelope représente le body POST sur `/events`. Format observé :
//
//	{"events":[{"ts":<epoch>,"ev":{...}}, ...]}
//
// Chaque `ev` contient au minimum un champ `type` (string) qui détermine
// la sémantique du reste. Types observés en live :
//
//   - shutter_open / shutter_close (caméra : volet objectif)
//   - day_alarm_delay_on / alarm_on / alarm_trigged / alarm_day_off /
//     alarm_night_off (transitions alarme — déjà capturées par le tail
//     fbxhome.log mais on les reçoit aussi ici)
//   - dws_open / dws_close (porte/fenêtre)
//   - false_alert, timeout_alert, timeout_before_alert
//   - pkt_lost
type EventEnvelope struct {
	Events []EventItem `json:"events"`
}

// EventItem est un event horodaté dans la queue.
type EventItem struct {
	Timestamp int64       `json:"ts"`
	Event     EventDetail `json:"ev"`
}

// EventDetail contient au moins `type`. Les autres champs varient :
// `area`, `room`, `item_id`, `name`, `node_type` pour les events sensor ;
// rien pour les events système. On utilise un map pour rester souple.
type EventDetail struct {
	Type    string         `json:"type"`
	Area    string         `json:"area,omitempty"`
	Room    string         `json:"room,omitempty"`
	ItemID  int            `json:"item_id,omitempty"`
	Name    string         `json:"name,omitempty"`
	NodeTy  string         `json:"node_type,omitempty"`
	TStamp  string         `json:"timestamp,omitempty"` // format "HH:MM:SS,MM-DD-YYYY"
	Extras  map[string]any `json:"-"`
}

// At retourne le timestamp Unix de l'event sous forme `time.Time`.
func (e EventItem) At() time.Time {
	return time.Unix(e.Timestamp, 0)
}

// NotificationEnvelope représente le body POST sur `/notifications`.
// Format observé :
//
//	{"notifications":[{"ts":<epoch>,"notif":{<contenu>}}, ...]}
//
// Les notifications portent un champ `type` au top-level du `notif` et
// un sous-objet `data` qui contient les détails. Type principal observé :
// `iv_event` (IntelliVision : détection humain/pet).
type NotificationEnvelope struct {
	Notifications []NotificationItem `json:"notifications"`
}

// NotificationItem est une notification horodatée.
type NotificationItem struct {
	Timestamp int64        `json:"ts"`
	Notif     Notification `json:"notif"`
}

// Notification décrit une notif. `Type` détermine quoi contient `Data`.
type Notification struct {
	Type string          `json:"type"`
	Data NotificationData `json:"data"`
}

// NotificationData pour les `iv_event` :
//
//	{"events":[{"eventType":"eObjectEnteredEvent","timestamp":...}],
//	 "objects":[{"class":"eOC_Human","confidence":1,"id":"26"}]}
//
// `events` peut être absent (certaines notifs n'ont que des objects).
// `objects` peut aussi être absent (events purs sans nouvel objet
// classifié, juste un Enter/Exit avec ID référencé d'une notif précédente).
type NotificationData struct {
	Events  []IVEvent  `json:"events,omitempty"`
	Objects []IVObject `json:"objects,omitempty"`
}

// IVEvent décrit une transition IntelliVision. EventType est l'un de :
//
//   - eObjectEnteredEvent : nouvel objet entré dans le champ
//   - eObjectExitedEvent  : objet sorti du champ
//   - eObjectClassified   : objet classifié (avec confidence dans le
//                           objet correspondant via ID)
//   - eObjectLost         : objet perdu de vue (sans Exit propre)
type IVEvent struct {
	EventType string `json:"eventType"`
	Timestamp int64  `json:"timestamp"`
}

// IVObject décrit un objet détecté/classifié par IntelliVision.
//
//   - Class : eOC_Human ou eOC_Pet (autres classes inconnues)
//   - Confidence : float [0..1], 1 = certitude
//   - ID : identifiant interne IV (réutilisé entre Enter / Classify / Exit)
type IVObject struct {
	Class      string  `json:"class"`
	Confidence float64 `json:"confidence"`
	ID         string  `json:"id"`
}

// Classes IntelliVision connues (utiliser ces constantes pour les
// comparaisons typées plutôt que des string literals dispersées).
const (
	IVClassHuman = "eOC_Human"
	IVClassPet   = "eOC_Pet"
)

// EventTypes IntelliVision connus.
const (
	IVEventEntered    = "eObjectEnteredEvent"
	IVEventExited     = "eObjectExitedEvent"
	IVEventClassified = "eObjectClassified"
	IVEventLost       = "eObjectLost"
)

// NotifType valeurs connues au top-level d'une notification.
const (
	NotifTypeIV = "iv_event"
)
