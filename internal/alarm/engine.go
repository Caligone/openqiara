// Package alarm implements the standalone alarm state machine.
//
// The engine maintains the current alarm state (disarmed / arming /
// armed_night / armed_away / pending / triggered), applies transitions in
// response to KPD commands and sensor events, and persists the state to
// disk across reboots.
//
// See docs/alarm.md (TODO) for the full state machine diagram.
package alarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// State represents the current alarm state.
type State string

const (
	StateDisarmed    State = "disarmed"
	StateArming      State = "arming"       // 60s grace period after arm, before surveillance starts
	StateArmedNight  State = "armed_night"
	StateArmedAway   State = "armed_away"
	StatePending     State = "pending"      // 60s grace period after sensor trigger, before siren
	StateTriggered   State = "triggered"    // siren active for up to 5 minutes
)

// Default timer durations. Engine uses overrides via SetTimings if provided.
const (
	DefaultArmingDelay  = 60 * time.Second
	DefaultPendingDelay = 60 * time.Second
	TriggeredDelay      = 5 * time.Minute
)

// persistedState is the on-disk representation.
type persistedState struct {
	State         State `json:"state"`
	ArmedAt       int64 `json:"armed_at,omitempty"`
	TriggeredBy   int   `json:"triggered_by,omitempty"`
	PreviousState State `json:"previous_state,omitempty"`
}

// Snapshot is an immutable view of the engine state, returned to callers.
type Snapshot struct {
	State         State `json:"state"`
	ArmedAt       int64 `json:"armed_at,omitempty"`
	TriggeredBy   int   `json:"triggered_by,omitempty"`
	PreviousState State `json:"previous_state,omitempty"`
	// TimerRemaining is seconds until the next automatic transition
	// (arming→armed, pending→triggered, triggered→previous). 0 if no timer.
	TimerRemaining int `json:"timer_remaining,omitempty"`
}

// SensorConfig tells the engine how a given sensor behaves in each mode.
type SensorConfig struct {
	// NightAllowed: if true, this sensor does NOT trigger in armed_night mode
	// (motion is allowed). All sensors trigger in armed_away regardless.
	// Default is false: the sensor is strict (triggers in both modes).
	NightAllowed bool
}

// ConfigProvider returns per-sensor configuration at runtime.
// Callers must implement this so the engine can query fresh config after UI changes.
type ConfigProvider func(sensorID int) SensorConfig

// StateChangeCallback is invoked whenever the engine transitions to a new state.
// It is called under the engine's lock, so callbacks must not call back into the engine.
type StateChangeCallback func(snap Snapshot)

// Engine is the alarm state machine.
type Engine struct {
	mu        sync.Mutex
	state     State
	armedAt   int64
	trigBy    int
	prevState State

	// timerDeadline is when the current timer fires; zero if no timer active.
	timerDeadline time.Time

	// armingDelay and pendingDelay are the per-instance timings; defaulted in New.
	armingDelay  time.Duration
	pendingDelay time.Duration

	// lastOpen tracks whether each sensor was "in alarm" last time we saw it,
	// to implement the "armed with door already open → ignore until it closes
	// and reopens" rule.
	lastAlarm map[int]bool

	path       string
	configFor  ConfigProvider
	onChange   StateChangeCallback
	logger     *slog.Logger
	done       chan struct{}
	wg         sync.WaitGroup
}

// New creates a new alarm engine. It does NOT start the timer loop — call Start.
// path is where the state JSON is persisted.
func New(path string, configFor ConfigProvider, onChange StateChangeCallback, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	if configFor == nil {
		configFor = func(int) SensorConfig { return SensorConfig{} }
	}
	return &Engine{
		state:        StateDisarmed,
		path:         path,
		configFor:    configFor,
		onChange:     onChange,
		logger:       logger,
		lastAlarm:    make(map[int]bool),
		done:         make(chan struct{}),
		armingDelay:  DefaultArmingDelay,
		pendingDelay: DefaultPendingDelay,
	}
}

// SetTimings overrides the default arming/pending delays. Pass 0 to keep
// the existing value. Safe to call before Start ou en runtime.
func (e *Engine) SetTimings(arming, pending time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if arming > 0 {
		e.armingDelay = arming
	}
	if pending > 0 {
		e.pendingDelay = pending
	}
	e.logger.Info("alarm: timings set", "arming", e.armingDelay, "pending", e.pendingDelay)
}

// Load restores the engine state from disk. Safe to call before Start.
// If the file does not exist, the engine remains in StateDisarmed.
// Transient states (arming, pending) are resolved to their stable parent.
func (e *Engine) Load() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	data, err := os.ReadFile(e.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("alarm: read state: %w", err)
	}
	var p persistedState
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("alarm: parse state: %w", err)
	}

	// Resolve transient states on boot:
	// - arming → previous stable state (or disarmed)
	// - pending → armed_* (from previous_state)
	// - triggered → restore as-is, timer will fire 5min from loading
	// - armed_* / disarmed → as-is
	switch p.State {
	case StateArming:
		e.state = StateDisarmed
	case StatePending:
		if p.PreviousState == StateArmedNight || p.PreviousState == StateArmedAway {
			e.state = p.PreviousState
		} else {
			e.state = StateDisarmed
		}
	case StateTriggered:
		e.state = StateTriggered
		e.prevState = p.PreviousState
		e.timerDeadline = time.Now().Add(TriggeredDelay)
	case StateArmedAway, StateArmedNight:
		e.state = p.State
		e.armedAt = p.ArmedAt
	case StateDisarmed, "":
		e.state = StateDisarmed
	default:
		e.state = StateDisarmed
	}

	e.trigBy = p.TriggeredBy
	e.logger.Info("alarm: state loaded", "state", e.state, "previous", e.prevState)
	return nil
}

// Start begins the background timer loop. Call Stop to shut down.
func (e *Engine) Start() {
	e.wg.Add(1)
	go e.timerLoop()
}

// Stop gracefully shuts down the engine.
func (e *Engine) Stop() {
	close(e.done)
	e.wg.Wait()
}

// Snapshot returns the current state.
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshotLocked()
}

// Source identifie l'origine d'une commande utilisateur, pour adapter le
// comportement (notamment le délai d'armement).
type Source string

const (
	// SourceLocal : commande émise depuis un dispositif physique sur place
	// (KPD). L'utilisateur a besoin du délai d'armement pour quitter les
	// lieux avant que la surveillance ne démarre.
	SourceLocal Source = "local"
	// SourceRemote : commande émise à distance (HK, web UI, MQTT/HA).
	// L'utilisateur n'est pas sur place — surveiller immédiatement, sans
	// délai d'armement qui laisserait un intrus s'échapper.
	SourceRemote Source = "remote"
)

// HandleCommand processes a user command. Valid commands: "arm_away",
// "arm_night", "disarm". `source` détermine si on applique le délai d'armement
// (uniquement pour SourceLocal).
func (e *Engine) HandleCommand(cmd string, source Source) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Helper : arme directement (skip délai) si remote, sinon démarre arming.
	// Remote = strict immédiat → check post-arm pour capteur déjà en alarme.
	// Local = délai d'armement, le check se fait à expiration du timer.
	armTo := func(target State) {
		if source == SourceRemote {
			e.transitionLocked(target, "remote arm (no delay)")
			e.triggerIfSensorInAlarmLocked()
		} else {
			e.startArming(target)
		}
	}

	switch cmd {
	case "disarm":
		// Disarm from any state → disarmed. Pas de délai dans tous les cas.
		if e.state != StateDisarmed {
			e.transitionLocked(StateDisarmed, "user disarm")
		}

	case "arm_away":
		switch e.state {
		case StateDisarmed:
			armTo(StateArmedAway)
		case StateArmedNight:
			// Switch mode without re-arming delay (déjà armé).
			e.transitionLocked(StateArmedAway, "switch to away")
		case StateArmedAway:
			// No-op — already in this mode.
		case StateArming, StatePending, StateTriggered:
			// Ignore — user must disarm first.
		}

	case "arm_night":
		switch e.state {
		case StateDisarmed:
			armTo(StateArmedNight)
		case StateArmedAway:
			e.transitionLocked(StateArmedNight, "switch to night")
		case StateArmedNight:
			// No-op.
		case StateArming, StatePending, StateTriggered:
			// Ignore.
		}

	default:
		e.logger.Warn("alarm: unknown command", "cmd", cmd)
	}
}

// HandleSensorEvent processes a sensor state change (DWS open, PIR motion, etc.).
// `inAlarm` indicates whether the sensor is currently in its "alarm" state
// (e.g. DWS open, PIR motion detected). The caller is responsible for deciding
// what counts as "in alarm" for each sensor type.
func (e *Engine) HandleSensorEvent(sensorID int, sensorType string, inAlarm bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Update the transition tracker regardless of state — we need it for the
	// "ignore sensor that was already in alarm when we armed" rule.
	prevAlarm := e.lastAlarm[sensorID]
	e.lastAlarm[sensorID] = inAlarm

	// KPD never triggers the alarm (it's the control device).
	if sensorType == "KPD" {
		return
	}

	// Only armed_* states can be triggered by sensors.
	if e.state != StateArmedNight && e.state != StateArmedAway {
		return
	}

	// Tout capteur en alarme (transition ou état persistant) déclenche.
	// La grâce "capteur déjà ouvert au moment de l'arm" est désormais gérée
	// par le délai d'armement (StateArming, Source=Local uniquement) :
	// pendant ce délai les events sensors sont ignorés, et à expiration
	// un snapshot vérifie l'état réel (cf. tick() / armingExpired).
	if !inAlarm {
		return
	}
	_ = prevAlarm // conservé pour debug, plus utilisé en logique

	// In armed_night mode, a sensor with NightAllowed=true does not trigger.
	// In armed_away mode, all sensors trigger.
	cfg := e.configFor(sensorID)
	if e.state == StateArmedNight && cfg.NightAllowed {
		return
	}

	// Trigger: transition to pending (60s grace before siren).
	e.trigBy = sensorID
	e.logger.Info("alarm: sensor triggered", "sensor_id", sensorID, "type", sensorType, "state", e.state)
	e.startPending()
}

// --- private helpers (all called with e.mu held) ---

func (e *Engine) startArming(target State) {
	// Record "previous state" so the triggered→previous rollback works.
	e.prevState = target
	e.state = StateArming
	e.timerDeadline = time.Now().Add(e.armingDelay)
	e.notifyLocked("start arming")
	e.saveLocked()
}

// triggerIfSensorInAlarmLocked : à appeler après une transition vers
// armed_away/armed_night pour vérifier si un capteur est déjà en alarme.
// Si oui, déclenche pending immédiatement. Premier capteur trouvé l'emporte.
// Requiert e.mu déjà lock.
func (e *Engine) triggerIfSensorInAlarmLocked() {
	if e.state != StateArmedAway && e.state != StateArmedNight {
		return
	}
	for sensorID, inAlarm := range e.lastAlarm {
		if !inAlarm {
			continue
		}
		cfg := e.configFor(sensorID)
		if e.state == StateArmedNight && cfg.NightAllowed {
			continue
		}
		e.trigBy = sensorID
		e.logger.Info("alarm: sensor still in alarm post-arm", "sensor_id", sensorID, "state", e.state)
		e.startPending()
		return
	}
}

func (e *Engine) startPending() {
	// prevState is already set to the armed state we came from.
	e.prevState = e.state
	e.state = StatePending
	e.timerDeadline = time.Now().Add(e.pendingDelay)
	e.notifyLocked("start pending")
	e.saveLocked()
}

// transitionLocked changes state directly, sets timer if applicable, persists, notifies.
func (e *Engine) transitionLocked(newState State, reason string) {
	old := e.state
	e.state = newState

	switch newState {
	case StateDisarmed:
		e.armedAt = 0
		e.trigBy = 0
		e.prevState = ""
		e.timerDeadline = time.Time{}
		// Reset the "already in alarm" tracker so next arm starts fresh.
		e.lastAlarm = make(map[int]bool)
	case StateArmedAway, StateArmedNight:
		if old == StateArming {
			e.armedAt = time.Now().Unix()
		}
		if e.armedAt == 0 {
			e.armedAt = time.Now().Unix()
		}
		e.timerDeadline = time.Time{}
	case StateTriggered:
		// Entered from pending — prevState is the armed_* we came from.
		e.timerDeadline = time.Now().Add(TriggeredDelay)
	}

	e.logger.Info("alarm: state transition", "from", old, "to", newState, "reason", reason)
	e.notifyLocked(reason)
	e.saveLocked()
}

func (e *Engine) snapshotLocked() Snapshot {
	snap := Snapshot{
		State:         e.state,
		ArmedAt:       e.armedAt,
		TriggeredBy:   e.trigBy,
		PreviousState: e.prevState,
	}
	if !e.timerDeadline.IsZero() {
		remaining := time.Until(e.timerDeadline)
		if remaining < 0 {
			remaining = 0
		}
		snap.TimerRemaining = int(remaining.Seconds())
	}
	return snap
}

func (e *Engine) notifyLocked(_ string) {
	if e.onChange != nil {
		snap := e.snapshotLocked()
		// Callback runs under the lock — caller must not call back into the engine.
		e.onChange(snap)
	}
}

func (e *Engine) saveLocked() {
	p := persistedState{
		State:         e.state,
		ArmedAt:       e.armedAt,
		TriggeredBy:   e.trigBy,
		PreviousState: e.prevState,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		e.logger.Error("alarm: marshal state", "error", err)
		return
	}
	if err := os.WriteFile(e.path, data, 0644); err != nil {
		e.logger.Error("alarm: write state", "error", err)
	}
}

// timerLoop handles automatic transitions (arming→armed, pending→triggered, triggered→previous).
func (e *Engine) timerLoop() {
	defer e.wg.Done()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-e.done:
			return
		case <-t.C:
			e.tick()
		}
	}
}

func (e *Engine) tick() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.timerDeadline.IsZero() || time.Now().Before(e.timerDeadline) {
		return
	}

	switch e.state {
	case StateArming:
		// arming → armed_*. prevState holds the target mode.
		target := e.prevState
		if target != StateArmedNight && target != StateArmedAway {
			target = StateArmedAway
		}
		e.transitionLocked(target, "arming timer expired")
		// Snapshot post-arming : si un capteur est encore en alarme,
		// déclencher immédiatement (la grâce vient de finir).
		e.triggerIfSensorInAlarmLocked()

	case StatePending:
		// pending → triggered.
		e.transitionLocked(StateTriggered, "pending timer expired")

	case StateTriggered:
		// triggered → back to previous armed state (or disarmed if none).
		rollback := e.prevState
		if rollback != StateArmedNight && rollback != StateArmedAway {
			rollback = StateDisarmed
		}
		e.transitionLocked(rollback, "triggered timer expired")
	}
}
