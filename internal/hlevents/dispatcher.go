package hlevents

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Detection représente un objet détecté en cours de présence dans le
// champ de la caméra. Émis par le Dispatcher quand un eObjectEnteredEvent
// arrive (ou la première fois qu'on voit un ID), et révoqué après timeout
// sans event Exit/Lost.
type Detection struct {
	Class      string  // IVClassHuman / IVClassPet
	Confidence float64 // 0..1, mis à jour par eObjectClassified
	ObjectID   string  // ID interne IV
	Present    bool    // true = en cours, false = parti
}

// DetectionSink reçoit les changements de détection (Present true/false).
// Idempotent : la même Detection peut être émise plusieurs fois (mise à
// jour de Confidence, par exemple) ; le sink doit dédoublonner si besoin.
type DetectionSink func(Detection)

// Dispatcher traite les notifications IV et émet des Detection sur le sink.
// Maintient un état interne par objet (ID → class/confidence/last_seen) et
// auto-expire après ExitTimeout sans event Exit explicite — utile pour les
// cas où IV log un Enter sans Exit (cam qui perd l'objet à mi-frame).
type Dispatcher struct {
	logger      *slog.Logger
	sink        DetectionSink
	exitTimeout time.Duration

	mu     sync.Mutex
	state  map[string]*Detection // par ObjectID
	timers map[string]*time.Timer
}

// NewDispatcher construit un dispatcher. exitTimeout = combien de temps
// sans event Exit/Lost avant de considérer l'objet parti (typique 30s).
func NewDispatcher(logger *slog.Logger, sink DetectionSink, exitTimeout time.Duration) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if exitTimeout <= 0 {
		exitTimeout = 30 * time.Second
	}
	return &Dispatcher{
		logger:      logger,
		sink:        sink,
		exitTimeout: exitTimeout,
		state:       make(map[string]*Detection),
		timers:      make(map[string]*time.Timer),
	}
}

// HandleNotification traite une notification reçue depuis le push HTTP.
// Seules les notifs de type iv_event sont traitées aujourd'hui ; les
// autres types sont loggués au niveau debug pour qu'on puisse les exposer
// plus tard si besoin.
func (d *Dispatcher) HandleNotification(_ context.Context, item NotificationItem) {
	switch item.Notif.Type {
	case NotifTypeIV:
		d.handleIV(item)
	default:
		d.logger.Debug("hlevents: unknown notif type",
			"type", item.Notif.Type, "ts", item.Timestamp)
	}
}

// HandleEvent traite un event /events. Pour l'instant on ne fait que
// logger — les events sensor/alarm sont déjà capturés par d'autres voies
// (fbxhomelog tail). À étendre si on veut consommer shutter_open/close
// d'ici plutôt qu'ailleurs.
func (d *Dispatcher) HandleEvent(_ context.Context, item EventItem) {
	d.logger.Debug("hlevents: /events item",
		"type", item.Event.Type, "ts", item.Timestamp,
		"item_id", item.Event.ItemID, "node_type", item.Event.NodeTy)
}

// handleIV met à jour l'état interne pour chaque event + chaque object.
// Logique :
//
//   - eObjectEnteredEvent → enregistre l'ID, marque Present=true
//   - eObjectClassified   → met à jour Class/Confidence pour l'ID
//   - eObjectExitedEvent  → marque Present=false
//   - eObjectLost         → marque Present=false (objet perdu de vue)
//
// Les `objects` reçus dans une notif sont appliqués à TOUS les events de
// cette notif (le payload n'a pas de mapping explicite event→object, mais
// les IDs concordent — la cam regroupe par objet courant).
func (d *Dispatcher) handleIV(item NotificationItem) {
	data := item.Notif.Data

	// Update class/confidence à partir des objects présents.
	d.mu.Lock()
	for _, obj := range data.Objects {
		if obj.ID == "" {
			continue
		}
		det := d.state[obj.ID]
		if det == nil {
			det = &Detection{ObjectID: obj.ID, Class: obj.Class, Confidence: obj.Confidence, Present: true}
			d.state[obj.ID] = det
		} else {
			det.Class = obj.Class
			det.Confidence = obj.Confidence
		}
	}
	d.mu.Unlock()

	// Apply events. Si la notif n'a que des objects sans events
	// (eObjectClassified pur), on émet quand même une mise à jour
	// Present=true depuis le bloc précédent — sauf si l'objet est
	// déjà connu (on évite le bruit).
	for _, ev := range data.Events {
		d.applyIVEvent(ev, data.Objects)
	}

	// Si pas d'events mais des objects, c'est une notif "classification"
	// d'un objet déjà entré. On émet quand même la Detection mise à jour
	// (avec Present=true) pour que le sink remette son timer.
	if len(data.Events) == 0 && len(data.Objects) > 0 {
		d.mu.Lock()
		for _, obj := range data.Objects {
			if det := d.state[obj.ID]; det != nil {
				det.Present = true
				d.emitLocked(det)
				d.resetExitTimerLocked(obj.ID)
			}
		}
		d.mu.Unlock()
	}
}

// applyIVEvent applique un IVEvent. Utilise les objects passés en
// paramètre pour résoudre la classe si l'event n'a pas d'ID explicite.
func (d *Dispatcher) applyIVEvent(ev IVEvent, objects []IVObject) {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch ev.EventType {
	case IVEventEntered, IVEventClassified:
		// Si pas d'objet associé à cet event, on applique à tous les
		// objects de la notif (cas observé : entered + 1 objet avec ID).
		for _, obj := range objects {
			det := d.state[obj.ID]
			if det == nil {
				det = &Detection{ObjectID: obj.ID, Class: obj.Class, Confidence: obj.Confidence, Present: true}
				d.state[obj.ID] = det
			}
			det.Present = true
			d.emitLocked(det)
			d.resetExitTimerLocked(obj.ID)
		}
		// Cas pas d'objects dans la notif : on n'a pas d'ID, on ignore
		// (on attendra le prochain event avec object).
		if len(objects) == 0 {
			d.logger.Debug("hlevents: IV enter/classify without objects", "ts", ev.Timestamp)
		}

	case IVEventExited, IVEventLost:
		// Marquer toutes les detections actuelles comme parties si
		// l'event n'a pas d'ID — pessimiste mais sûr.
		if len(objects) == 0 {
			// Cas observé : eObjectExitedEvent seul, sans objects.
			// On désactive toutes les détections présentes — c'est la
			// sémantique observée : un Exit = scène vide.
			for id, det := range d.state {
				if det.Present {
					det.Present = false
					d.emitLocked(det)
					d.cancelExitTimerLocked(id)
				}
			}
		} else {
			for _, obj := range objects {
				if det := d.state[obj.ID]; det != nil {
					det.Present = false
					d.emitLocked(det)
					d.cancelExitTimerLocked(obj.ID)
				}
			}
		}

	default:
		d.logger.Debug("hlevents: unknown IV event type", "type", ev.EventType)
	}
}

// emitLocked appelle le sink. Doit être appelé avec d.mu Lock'é.
// On copie la Detection pour que le sink ne se retrouve pas avec un
// pointeur sur la state interne mutable.
func (d *Dispatcher) emitLocked(det *Detection) {
	if d.sink == nil {
		return
	}
	cp := *det
	go d.sink(cp)
}

// resetExitTimerLocked (re)démarre le timer auto-expire pour cet ID.
// À appeler quand on voit un Enter ou Classify (preuve de vie).
func (d *Dispatcher) resetExitTimerLocked(id string) {
	if t, ok := d.timers[id]; ok && t != nil {
		t.Stop()
	}
	d.timers[id] = time.AfterFunc(d.exitTimeout, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if det, ok := d.state[id]; ok && det.Present {
			det.Present = false
			d.logger.Info("hlevents: IV auto-expire", "id", id, "class", det.Class)
			d.emitLocked(det)
		}
		delete(d.timers, id)
	})
}

// cancelExitTimerLocked annule le timer auto-expire. À appeler sur Exit/Lost.
func (d *Dispatcher) cancelExitTimerLocked(id string) {
	if t, ok := d.timers[id]; ok && t != nil {
		t.Stop()
		delete(d.timers, id)
	}
}

// Reset (utile en tests) nettoie l'état interne.
func (d *Dispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.timers {
		if t != nil {
			t.Stop()
		}
	}
	d.state = make(map[string]*Detection)
	d.timers = make(map[string]*time.Timer)
}
