// Package alarm implements the standalone alarm and the driver of the
// siren (siren.go), which both alarm modes share.
//
// The siren keeps the alarm, as it did under fbxhome: armed, it counts the
// exit and entry delays, plays their beeps and wails, and it hears the
// sensors itself when the gateway is gone. The engine stays the authority
// on the alarm's state: it arms the siren, tells it the entry delay or the
// alert, and counts the delays too, a little longer than the siren. The
// siren's reports can only move the alarm forward (armed, entry delay, set
// off), never back: a set-off alarm leaves that state only when disarmed.
// A delay that ends without the siren's report (torn off, a report lost)
// moves the alarm on all the same. Without a siren, the engine's own delays
// are the alarm's.
//
// The engine also keeps what the siren does not know: the mode (away or
// night), when it was armed and which sensor set it off, persisted across
// reboots.
package alarm

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"
)

// State represents the current alarm state.
type State string

const (
	StateDisarmed   State = "disarmed"
	StateArming     State = "arming" // exit delay
	StateArmedNight State = "armed_night"
	StateArmedAway  State = "armed_away"
	StatePending    State = "pending"   // entry delay
	StateTriggered  State = "triggered" // set off, until disarmed
)

// Default delays.
const (
	DefaultArmingDelay  = 60 * time.Second
	DefaultPendingDelay = 60 * time.Second
)

// sirenMargin is how much longer than the siren the engine waits at the
// end of a delay, for the siren to report it first. A variable for tests.
var sirenMargin = 5 * time.Second

// persistedState is the on-disk representation.
type persistedState struct {
	State       State `json:"state"`
	Mode        State `json:"mode,omitempty"` // armed_away or armed_night, set while not disarmed
	ArmedAt     int64 `json:"armed_at,omitempty"`
	TriggeredBy int   `json:"triggered_by,omitempty"`
	// PostSilence: the siren was silenced remotely, detection is instant
	// until the alarm is disarmed.
	PostSilence bool `json:"post_silence,omitempty"`
	// PreviousState is the old name of Mode, read for files written before.
	PreviousState State `json:"previous_state,omitempty"`
}

// Snapshot is an immutable view of the engine state, returned to callers.
type Snapshot struct {
	State         State `json:"state"`
	ArmedAt       int64 `json:"armed_at,omitempty"`
	TriggeredBy   int   `json:"triggered_by,omitempty"`
	PreviousState State `json:"previous_state,omitempty"` // the armed mode
	// TimerRemaining is the seconds left in the exit or entry delay; 0
	// otherwise.
	TimerRemaining int `json:"timer_remaining,omitempty"`
}

// SensorConfig tells the engine how a given sensor behaves in each mode.
type SensorConfig struct {
	// NightAllowed: the sensor is ignored when armed for the night.
	NightAllowed bool
	// Instant: the sensor sets the alarm off with no entry delay (a
	// window: nobody comes in through it).
	Instant bool
}

// ConfigProvider returns per-sensor configuration at runtime.
type ConfigProvider func(sensorID int) SensorConfig

// StateChangeCallback is invoked whenever the engine transitions to a new state.
// It is called under the engine's lock, so callbacks must not call back into the engine.
type StateChangeCallback func(snap Snapshot)

// Siren is the siren as the engine commands it (SirenDriver).
type Siren interface {
	Present() bool
	Arm(night, delayed bool)
	Disarm()
	EntryDelay()
	Alert()
}

// SirenState is a state the siren reports.
type SirenState string

const (
	SirenOff        SirenState = "off"
	SirenTest       SirenState = "test"
	SirenExitDelay  SirenState = "exit_delay"
	SirenArmed      SirenState = "armed"
	SirenEntryDelay SirenState = "entry_delay"
	SirenAlert      SirenState = "alert"
	SirenAlertOver  SirenState = "alert_over" // alert ended, still set off
)

// Alert is the alarm set off, or set off again, by a sensor.
type Alert struct {
	SensorID int  // 0: the siren set itself off (no sensor known)
	Again    bool // a new detection while already triggered
}

// Engine is the alarm.
type Engine struct {
	mu       sync.Mutex
	state    State
	mode     State // armed_away or armed_night while not disarmed
	armedAt  int64
	trigBy   int
	deadline time.Time // end of the delay in progress
	timerGen int       // bumped by every transition: stale timers do nothing
	closed   bool      // Close: no more writes

	armingDelay  time.Duration
	pendingDelay time.Duration

	// lastAlarm is whether each sensor is in alarm (door open, motion), so
	// that a sensor still in alarm when the exit delay ends sets it off.
	lastAlarm map[int]bool

	// Silence: sensors that were in alarm when the siren was silenced stay
	// ignored until they rest, so that a door left open does not set the
	// alarm off again at once.
	ignored map[int]bool
	// postSilence: every sensor sets the alarm off with no entry delay,
	// nobody being expected home, until the alarm is disarmed.
	postSilence bool
	// awaitSirenReset: the siren has not yet reported the re-arming after a
	// silence, so an alert it reports is the old one, sent before.
	awaitSirenReset bool

	siren     Siren
	path      string
	configFor ConfigProvider
	onChange  StateChangeCallback
	alertHook func(Alert)
	logger    *slog.Logger
}

// New creates the engine.
func New(path string, siren Siren, configFor ConfigProvider, onChange StateChangeCallback, logger *slog.Logger) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	if configFor == nil {
		configFor = func(int) SensorConfig { return SensorConfig{} }
	}
	return &Engine{
		state:        StateDisarmed,
		siren:        siren,
		path:         path,
		configFor:    configFor,
		onChange:     onChange,
		logger:       logger,
		lastAlarm:    make(map[int]bool),
		ignored:      make(map[int]bool),
		armingDelay:  DefaultArmingDelay,
		pendingDelay: DefaultPendingDelay,
	}
}

// SetAlertHook sets the function called when the alarm is set off, and at
// each new detection while it is. fn runs under the engine's lock: it must
// not block nor call the engine. It is not called on a silence or a disarm.
func (e *Engine) SetAlertHook(fn func(Alert)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.alertHook = fn
}

// SetTimings sets the exit and entry delays. 0 keeps the current value.
func (e *Engine) SetTimings(arming, pending time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if arming > 0 {
		e.armingDelay = arming
	}
	if pending > 0 {
		e.pendingDelay = pending
	}
}

// Load restores the engine state from disk and tells the siren what to
// want. An exit delay cut by the restart ends armed; an entry delay starts
// again, so that it still ends set off.
func (e *Engine) Load() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	data, err := os.ReadFile(e.path)
	if errors.Is(err, os.ErrNotExist) {
		e.siren.Disarm()
		return nil
	}
	if err != nil {
		e.siren.Disarm() // starting disarmed
		return fmt.Errorf("alarm: read state: %w", err)
	}
	var p persistedState
	if err := json.Unmarshal(data, &p); err != nil {
		e.siren.Disarm() // starting disarmed
		return fmt.Errorf("alarm: parse state: %w", err)
	}
	mode := p.Mode
	if mode == "" {
		mode = p.PreviousState
	}
	if mode != StateArmedNight {
		mode = StateArmedAway
	}
	switch p.State {
	case StateArming, StateArmedAway, StateArmedNight:
		e.state, e.mode, e.armedAt = mode, mode, p.ArmedAt
	case StatePending, StateTriggered:
		e.state, e.mode, e.armedAt, e.trigBy = p.State, mode, p.ArmedAt, p.TriggeredBy
	default:
		e.state = StateDisarmed
	}
	e.logger.Info("alarm: state loaded", "state", e.state, "mode", e.mode)
	if e.state == StateDisarmed {
		e.siren.Disarm()
		return nil
	}
	e.postSilence = p.PostSilence
	e.siren.Arm(e.mode == StateArmedNight, false)
	if e.state == StatePending {
		e.deadline = time.Now().Add(e.pendingDelay)
		e.entryTimerLocked()
	}
	return nil
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

// HandleCommand processes a user command: "arm_away", "arm_night" or
// "disarm". Only a local command (the keypad) gets the exit delay.
func (e *Engine) HandleCommand(cmd string, source Source) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch cmd {
	case "disarm":
		if e.state == StateDisarmed {
			return
		}
		e.transitionLocked(StateDisarmed, "user disarm")
		e.siren.Disarm()

	case "arm_away", "arm_night":
		mode := StateArmedAway
		if cmd == "arm_night" {
			mode = StateArmedNight
		}
		e.armLocked(mode, source == SourceLocal, false)

	default:
		e.logger.Warn("alarm: unknown command", "cmd", cmd)
	}
}

// ArmResult tells what a remote arming did.
type ArmResult string

const (
	ArmDone     ArmResult = "armed"    // armed now (from disarmed)
	ArmSwitched ArmResult = "switched" // was armed in the other mode, now in this one
	ArmAlready  ArmResult = "already"  // already armed in this mode: nothing done
	ArmBusy     ArmResult = "busy"     // exit delay, entry delay or set off: nothing done
)

// ArmRemote arms from afar: no exit delay (SourceRemote). With ignoreOpen,
// the watched sensors in alarm at that moment are ignored until they rest
// (the same set as after Silence), instead of setting the alarm off at once.
func (e *Engine) ArmRemote(night, ignoreOpen bool) ArmResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	mode := StateArmedAway
	if night {
		mode = StateArmedNight
	}
	return e.armLocked(mode, false, ignoreOpen)
}

// InAlarm returns, sorted, the sensors in alarm now that the given mode
// watches (a night arming leaves the night-allowed ones out).
func (e *Engine) InAlarm(night bool) []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ids []int
	for id, inAlarm := range e.lastAlarm {
		if inAlarm && e.watchedInLocked(id, night) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// armLocked arms the alarm in mode, or switches its mode. delayed: with the
// exit delay. ignoreOpen: the watched sensors in alarm are ignored until
// they rest.
func (e *Engine) armLocked(mode State, delayed, ignoreOpen bool) ArmResult {
	switch e.state {
	case StateDisarmed:
		e.mode, e.trigBy = mode, 0
		if ignoreOpen {
			e.ignoreInAlarmLocked()
		}
		// Armed before any alert is sent to it: an unarmed siren ignores it.
		e.siren.Arm(mode == StateArmedNight, delayed)
		if !delayed {
			e.armedLocked("armed")
			return ArmDone
		}
		e.transitionLocked(StateArming, "arming")
		e.afterLocked(e.armingDelay+e.margin(), func() {
			if e.state == StateArming {
				e.armedLocked("exit delay over")
			}
		})
		return ArmDone
	case StateArmedAway, StateArmedNight:
		if e.state == mode {
			return ArmAlready
		}
		e.mode = mode
		if ignoreOpen {
			e.ignoreInAlarmLocked()
		}
		e.transitionLocked(mode, "switch mode")
		e.siren.Arm(mode == StateArmedNight, false)
		e.inAlarmLocked() // night to away: a sensor now watched
		return ArmSwitched
	default:
		// Arming, pending or triggered: disarm first.
		return ArmBusy
	}
}

// ignoreInAlarmLocked ignores, until they rest, the watched sensors in alarm
// (e.mode is already the mode to arm).
func (e *Engine) ignoreInAlarmLocked() {
	for id, inAlarm := range e.lastAlarm {
		if inAlarm && e.watchedLocked(id) {
			e.ignored[id] = true
		}
	}
}

// armedLocked enters the armed mode, then sets the alarm off for a sensor
// still in alarm.
func (e *Engine) armedLocked(reason string) {
	e.armedAt = time.Now().Unix()
	e.transitionLocked(e.mode, reason)
	e.inAlarmLocked()
}

// inAlarmLocked sets the alarm off for a watched sensor still in alarm.
func (e *Engine) inAlarmLocked() {
	for id, inAlarm := range e.lastAlarm {
		if inAlarm && !e.ignored[id] && e.watchedLocked(id) {
			e.logger.Info("alarm: sensor still in alarm once armed", "sensor_id", id)
			e.sensorAlarmLocked(id)
			return
		}
	}
}

// HandleSensorEvent processes a sensor state change. inAlarm is whether
// the sensor is in alarm (door open, motion).
func (e *Engine) HandleSensorEvent(sensorID int, sensorType string, inAlarm bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if sensorType == "KPD" {
		return // the keypad commands the alarm, it never sets it off
	}
	e.lastAlarm[sensorID] = inAlarm
	if !inAlarm {
		delete(e.ignored, sensorID) // at rest: watched again
		return
	}
	if e.ignored[sensorID] || !e.watchedLocked(sensorID) {
		return
	}
	switch e.state {
	case StateArmedAway, StateArmedNight, StatePending, StateTriggered:
		e.sensorAlarmLocked(sensorID)
	}
}

// watchedLocked tells whether a sensor sets the alarm off in its mode.
func (e *Engine) watchedLocked(sensorID int) bool {
	return e.watchedInLocked(sensorID, e.mode == StateArmedNight)
}

// watchedInLocked tells whether a sensor sets the alarm off in the given
// mode (night or away).
func (e *Engine) watchedInLocked(sensorID int, night bool) bool {
	return !night || !e.configFor(sensorID).NightAllowed
}

// sensorAlarmLocked moves the alarm on for a sensor in alarm: the entry
// delay for a delayed sensor while armed, the alert for an instant one or
// once the alarm is under way, a delayed one during the entry delay
// excepted. The siren is told the same, as fbxhome told it.
func (e *Engine) sensorAlarmLocked(sensorID int) {
	e.logger.Info("alarm: sensor in alarm", "sensor_id", sensorID, "state", e.state)
	instant := e.configFor(sensorID).Instant || e.postSilence
	switch e.state {
	case StateArmedAway, StateArmedNight:
		e.trigBy = sensorID
		if instant {
			e.triggerLocked("instant sensor")
			return
		}
		e.transitionLocked(StatePending, "entry delay")
		e.siren.EntryDelay()
		e.entryTimerLocked()
	case StatePending:
		if instant {
			e.triggerLocked("instant sensor")
		}
	case StateTriggered:
		e.siren.Alert() // again, after an alert that ended
		if e.alertHook != nil {
			e.alertHook(Alert{SensorID: sensorID, Again: true})
		}
	}
}

// entryTimerLocked sets the alarm off when the entry delay ends without
// the siren's report.
func (e *Engine) entryTimerLocked() {
	e.afterLocked(time.Until(e.deadline)+e.margin(), func() {
		if e.state == StatePending {
			e.triggerLocked("entry delay over")
		}
	})
}

func (e *Engine) triggerLocked(reason string) {
	e.transitionLocked(StateTriggered, reason)
	e.siren.Alert()
}

// HandleSirenState moves the alarm on as the siren reports: the end of its
// exit delay, an entry delay or an alert it started itself (the gateway
// was gone). It never moves the alarm back: a late or stray report, or a
// siren that rebooted, changes nothing (SirenDriver brings the siren back).
func (e *Engine) HandleSirenState(s SirenState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.awaitSirenReset {
		switch s {
		case SirenAlert, SirenAlertOver:
			// Sent before the silence, received after: not a new alert.
			e.logger.Info("alarm: siren alert ignored, awaiting its re-arming", "siren", s)
			return
		case SirenOff, SirenArmed, SirenExitDelay:
			e.awaitSirenReset = false
		}
	}
	switch {
	case s == SirenArmed && e.state == StateArming:
		e.armedLocked("exit delay over")
	case s == SirenEntryDelay && (e.state == StateArmedAway || e.state == StateArmedNight):
		e.transitionLocked(StatePending, "siren entry delay")
		e.entryTimerLocked()
	case (s == SirenAlert || s == SirenAlertOver) && e.state != StateDisarmed && e.state != StateTriggered:
		e.transitionLocked(StateTriggered, "siren "+string(s))
	}
}

// Silence stops the siren and keeps the alarm armed, for a remote button.
// It returns false, with no effect, unless the alarm is set off. The siren
// is disarmed then armed again, which stops the wail, and the sensors in
// alarm are ignored until they rest. From then on any sensor sets the alarm
// off with no entry delay. It does not call inAlarmLocked: the sensors in
// alarm are the ones to ignore.
func (e *Engine) Silence() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state != StateTriggered {
		return false
	}
	e.siren.Disarm()
	e.siren.Arm(e.mode == StateArmedNight, false)
	for id, inAlarm := range e.lastAlarm {
		if inAlarm {
			e.ignored[id] = true
		}
	}
	e.postSilence, e.awaitSirenReset = true, true
	e.trigBy = 0 // armed again: no sensor holds the alarm
	e.transitionLocked(e.mode, "silenced remotely")
	return true
}

// margin is how much longer than the siren the engine counts a delay: 0
// without a siren, the engine's delays being the alarm's.
func (e *Engine) margin() time.Duration {
	if e.siren.Present() {
		return sirenMargin
	}
	return 0
}

// afterLocked runs fn under the lock after d, unless the state changed in
// the meantime.
func (e *Engine) afterLocked(d time.Duration, fn func()) {
	gen := e.timerGen
	time.AfterFunc(d, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.timerGen == gen {
			fn()
		}
	})
}

// Close stops the engine's timers and writes: its state file stays as it
// is.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.timerGen++
	e.closed = true
}

// transitionLocked changes state, persists, notifies.
func (e *Engine) transitionLocked(newState State, reason string) {
	old := e.state
	e.state = newState
	e.deadline = time.Time{}
	e.timerGen++
	switch newState {
	case StateDisarmed:
		e.armedAt, e.trigBy, e.mode = 0, 0, ""
		e.lastAlarm = make(map[int]bool)
		e.ignored = make(map[int]bool)
		e.postSilence, e.awaitSirenReset = false, false
	case StateArming:
		e.deadline = time.Now().Add(e.armingDelay)
	case StatePending:
		e.deadline = time.Now().Add(e.pendingDelay)
	}
	e.logger.Info("alarm: state transition", "from", old, "to", newState, "reason", reason)
	if e.onChange != nil {
		e.onChange(e.snapshotLocked())
	}
	e.saveLocked()
	if newState == StateTriggered && old != StateTriggered && e.alertHook != nil {
		e.alertHook(Alert{SensorID: e.trigBy})
	}
}

func (e *Engine) snapshotLocked() Snapshot {
	snap := Snapshot{State: e.state, ArmedAt: e.armedAt, TriggeredBy: e.trigBy, PreviousState: e.mode}
	if left := time.Until(e.deadline); !e.deadline.IsZero() && left > 0 {
		snap.TimerRemaining = int(left.Seconds())
	}
	return snap
}

func (e *Engine) saveLocked() {
	if e.closed {
		return
	}
	p := persistedState{State: e.state, Mode: e.mode, ArmedAt: e.armedAt, TriggeredBy: e.trigBy, PostSilence: e.postSilence}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		e.logger.Error("alarm: marshal state", "error", err)
		return
	}
	if err := os.WriteFile(e.path, data, 0644); err != nil {
		e.logger.Error("alarm: write state", "error", err)
	}
}
