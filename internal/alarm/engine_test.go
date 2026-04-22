package alarm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestEngine creates an engine with an in-memory state path and
// optional per-sensor config.
func newTestEngine(t *testing.T, cfg map[int]SensorConfig) *Engine {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "alarm.json")
	provider := func(id int) SensorConfig {
		if cfg == nil {
			return SensorConfig{}
		}
		return cfg[id]
	}
	return New(path, provider, nil, nil)
}

func TestInitialState(t *testing.T) {
	e := newTestEngine(t, nil)
	if s := e.Snapshot().State; s != StateDisarmed {
		t.Errorf("initial state = %q, want disarmed", s)
	}
}

func TestArmAwayGoesThroughArming(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	if s := e.Snapshot().State; s != StateArming {
		t.Errorf("after arm_away, state = %q, want arming", s)
	}
	if e.Snapshot().PreviousState != StateArmedAway {
		t.Errorf("previous state = %q, want armed_away", e.Snapshot().PreviousState)
	}
}

func TestArmingTimerExpires(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	// Force the timer to expire by setting the deadline in the past.
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()
	if s := e.Snapshot().State; s != StateArmedAway {
		t.Errorf("after arming expiry, state = %q, want armed_away", s)
	}
}

func TestDisarmFromAnyState(t *testing.T) {
	states := []string{"arm_away", "arm_night"}
	for _, cmd := range states {
		e := newTestEngine(t, nil)
		e.HandleCommand(cmd, SourceLocal)
		e.HandleCommand("disarm", SourceLocal)
		if s := e.Snapshot().State; s != StateDisarmed {
			t.Errorf("after %s + disarm, state = %q, want disarmed", cmd, s)
		}
	}
}

func TestSwitchBetweenArmedModes(t *testing.T) {
	e := newTestEngine(t, nil)
	// arm_away → wait → armed_away
	e.HandleCommand("arm_away", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()
	if s := e.Snapshot().State; s != StateArmedAway {
		t.Fatalf("setup failed: state = %q", s)
	}

	// Switching to night should be immediate (no arming delay).
	e.HandleCommand("arm_night", SourceLocal)
	if s := e.Snapshot().State; s != StateArmedNight {
		t.Errorf("after arm_night switch, state = %q, want armed_night", s)
	}

	// Back to away, also immediate.
	e.HandleCommand("arm_away", SourceLocal)
	if s := e.Snapshot().State; s != StateArmedAway {
		t.Errorf("after arm_away switch, state = %q, want armed_away", s)
	}
}

func TestSensorEventInDisarmedIgnored(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleSensorEvent(2, "DWS", true)
	if s := e.Snapshot().State; s != StateDisarmed {
		t.Errorf("sensor event in disarmed should not change state, got %q", s)
	}
}

func TestSensorEventInArmingIgnored(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.HandleSensorEvent(2, "DWS", true)
	if s := e.Snapshot().State; s != StateArming {
		t.Errorf("sensor event during arming should not trigger, got %q", s)
	}
}

func TestSensorEventInArmedAwayTriggersPending(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	// Skip arming delay.
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()

	e.HandleSensorEvent(2, "DWS", true)
	snap := e.Snapshot()
	if snap.State != StatePending {
		t.Errorf("after DWS trigger, state = %q, want pending", snap.State)
	}
	if snap.TriggeredBy != 2 {
		t.Errorf("triggered_by = %d, want 2", snap.TriggeredBy)
	}
}

func TestPendingTimerLeadsToTriggered(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // arming → armed_away
	e.HandleSensorEvent(2, "DWS", true)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // pending → triggered
	if s := e.Snapshot().State; s != StateTriggered {
		t.Errorf("after pending expiry, state = %q, want triggered", s)
	}
}

func TestDisarmFromPending(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()
	e.HandleSensorEvent(2, "DWS", true)
	if e.Snapshot().State != StatePending {
		t.Fatal("setup failed")
	}
	e.HandleCommand("disarm", SourceLocal)
	if s := e.Snapshot().State; s != StateDisarmed {
		t.Errorf("disarm from pending, state = %q, want disarmed", s)
	}
}

func TestTriggeredTimerRollsBack(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()
	e.HandleSensorEvent(2, "DWS", true)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // → triggered
	if e.Snapshot().State != StateTriggered {
		t.Fatal("setup failed")
	}
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // triggered timer → rollback to armed_away
	if s := e.Snapshot().State; s != StateArmedAway {
		t.Errorf("after triggered timeout, state = %q, want armed_away", s)
	}
}

func TestNightAllowedSensorIsIgnoredInNight(t *testing.T) {
	cfg := map[int]SensorConfig{
		2: {NightAllowed: false}, // DWS périmétrique, déclenche en nuit
		9: {NightAllowed: true},  // PIR intérieur, autorisé en nuit
	}
	e := newTestEngine(t, cfg)
	e.HandleCommand("arm_night", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // → armed_night

	// Sensor 9 (PIR, autorisé en nuit) — ne doit pas déclencher
	e.HandleSensorEvent(9, "PIR", true)
	if s := e.Snapshot().State; s != StateArmedNight {
		t.Errorf("NightAllowed PIR should not trigger in night mode, got %q", s)
	}

	// Sensor 2 (DWS, pas autorisé) — doit déclencher
	e.HandleSensorEvent(2, "DWS", true)
	if s := e.Snapshot().State; s != StatePending {
		t.Errorf("DWS without NightAllowed should trigger in night mode, got %q", s)
	}
}

func TestNightAllowedSensorStillTriggersInAway(t *testing.T) {
	cfg := map[int]SensorConfig{
		9: {NightAllowed: true}, // PIR autorisé en nuit
	}
	e := newTestEngine(t, cfg)
	e.HandleCommand("arm_away", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // → armed_away

	// En mode away, même le PIR NightAllowed doit déclencher.
	e.HandleSensorEvent(9, "PIR", true)
	if s := e.Snapshot().State; s != StatePending {
		t.Errorf("NightAllowed PIR should still trigger in away mode, got %q", s)
	}
}

func TestKPDEventNeverTriggers(t *testing.T) {
	e := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()
	// A KPD event should never trigger, even if inAlarm=true.
	e.HandleSensorEvent(20, "KPD", true)
	if s := e.Snapshot().State; s != StateArmedAway {
		t.Errorf("KPD event should never trigger, got %q", s)
	}
}

func TestSensorAlreadyInAlarmAtArmTimeTriggersPostDelay(t *testing.T) {
	// Source=Local : un capteur ouvert avant l'arm reste toléré pendant
	// le délai d'armement (le user a la grâce pour fermer la porte/sortir).
	// À expiration du délai, snapshot : si encore ouvert → trigger.
	e := newTestEngine(t, nil)
	e.HandleSensorEvent(2, "DWS", true)
	if e.Snapshot().State != StateDisarmed {
		t.Fatal("setup: must still be disarmed")
	}

	e.HandleCommand("arm_away", SourceLocal)
	if s := e.Snapshot().State; s != StateArming {
		t.Fatalf("expected arming during delay, got %q", s)
	}

	// Force expiration du délai d'armement.
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()

	// Capteur encore ouvert à expiration → trigger immédiat.
	if s := e.Snapshot().State; s != StatePending {
		t.Errorf("sensor still open at end of arming delay should trigger pending, got %q", s)
	}
}

func TestSensorAlreadyInAlarmRemoteArmTriggersImmediate(t *testing.T) {
	// Source=Remote : pas de grâce. Un capteur déjà ouvert au moment
	// de l'arm déclenche immédiatement (l'utilisateur n'est pas sur place).
	e := newTestEngine(t, nil)
	e.HandleSensorEvent(2, "DWS", true)

	e.HandleCommand("arm_away", SourceRemote)
	if s := e.Snapshot().State; s != StatePending {
		t.Errorf("remote arm with open sensor should trigger pending, got %q", s)
	}
}

func TestSensorOpenDuringArmingDelayClosedThenOK(t *testing.T) {
	// Si le capteur revient à normal pendant le délai d'armement, pas de trigger.
	e := newTestEngine(t, nil)
	e.HandleSensorEvent(2, "DWS", true)
	e.HandleCommand("arm_away", SourceLocal)

	// User ferme la porte avant la fin du délai.
	e.HandleSensorEvent(2, "DWS", false)

	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick()

	if s := e.Snapshot().State; s != StateArmedAway {
		t.Errorf("sensor closed during arming should not trigger, got %q", s)
	}
}

func TestLoadRestoresArmedAway(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alarm.json")
	// Write a persisted state directly.
	data := []byte(`{"state":"armed_away","armed_at":1712345678}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	e := New(path, nil, nil, nil)
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	if s := e.Snapshot().State; s != StateArmedAway {
		t.Errorf("after Load, state = %q, want armed_away", s)
	}
}

func TestLoadResolvesArmingToDisarmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alarm.json")
	data := []byte(`{"state":"arming","previous_state":"armed_away"}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	e := New(path, nil, nil, nil)
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	if s := e.Snapshot().State; s != StateDisarmed {
		t.Errorf("arming should be resolved to disarmed on load, got %q", s)
	}
}

func TestLoadResolvesPendingToPreviousArmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "alarm.json")
	data := []byte(`{"state":"pending","previous_state":"armed_night"}`)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	e := New(path, nil, nil, nil)
	if err := e.Load(); err != nil {
		t.Fatal(err)
	}
	if s := e.Snapshot().State; s != StateArmedNight {
		t.Errorf("pending should resolve to previous armed state, got %q", s)
	}
}

func TestCallbackFiresOnTransition(t *testing.T) {
	var got []State
	cb := func(snap Snapshot) { got = append(got, snap.State) }
	dir := t.TempDir()
	e := New(filepath.Join(dir, "alarm.json"), nil, cb, nil)

	e.HandleCommand("arm_away", SourceLocal) // → arming
	e.mu.Lock()
	e.timerDeadline = time.Now().Add(-time.Second)
	e.mu.Unlock()
	e.tick() // → armed_away
	e.HandleCommand("disarm", SourceLocal) // → disarmed

	want := []State{StateArming, StateArmedAway, StateDisarmed}
	if len(got) != len(want) {
		t.Fatalf("got %d callbacks, want %d: %v", len(got), len(want), got)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("callback[%d] = %q, want %q", i, got[i], s)
		}
	}
}

// TestRemoteArmSkipsArmingDelay vérifie qu'une commande distante (HK/web/MQTT)
// passe directement en state armé sans le délai d'armement.
//
// Rationale : depuis le KPD physique l'utilisateur a besoin de temps pour
// sortir (60s). Depuis HA / app mobile à distance, l'armement doit être
// immédiat sinon un intrus a 60s pour fuir après alerte.
func TestRemoteArmSkipsArmingDelay(t *testing.T) {
	dir := t.TempDir()
	e := New(filepath.Join(dir, "alarm.json"), nil, nil, nil)

	e.HandleCommand("arm_away", SourceRemote)
	snap := e.Snapshot()
	if snap.State != StateArmedAway {
		t.Errorf("remote arm_away → state %q, want %q (pas d'arming intermédiaire)", snap.State, StateArmedAway)
	}

	e.HandleCommand("disarm", SourceRemote)
	e.HandleCommand("arm_night", SourceRemote)
	snap = e.Snapshot()
	if snap.State != StateArmedNight {
		t.Errorf("remote arm_night → state %q, want %q", snap.State, StateArmedNight)
	}
}

// TestLocalArmKeepsArmingDelay vérifie qu'une commande locale (KPD) passe
// bien par StateArming avec son délai (régression pour la modif Source).
func TestLocalArmKeepsArmingDelay(t *testing.T) {
	dir := t.TempDir()
	e := New(filepath.Join(dir, "alarm.json"), nil, nil, nil)

	e.HandleCommand("arm_away", SourceLocal)
	snap := e.Snapshot()
	if snap.State != StateArming {
		t.Errorf("local arm_away → state %q, want %q (délai obligatoire)", snap.State, StateArming)
	}
}
