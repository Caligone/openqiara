package alarm

import (
	"path/filepath"
	"testing"
)

// sensor 1: window (instant); sensor 2: door (delayed).
var silenceCfg = map[int]SensorConfig{1: {Instant: true}, 2: {}}

func triggered(t *testing.T) (*Engine, *fakeSiren) {
	t.Helper()
	e, siren := newTestEngine(t, silenceCfg)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	expect(t, e, siren, StateTriggered, "arm", "alert")
	return e, siren
}

func TestSilenceKeepsArmed(t *testing.T) {
	e, siren := triggered(t)
	if !e.Silence() {
		t.Fatal("Silence() = false")
	}
	expect(t, e, siren, StateArmedAway, "disarm", "arm")
}

func TestSilenceAfterWailEnded(t *testing.T) {
	e, siren := triggered(t)
	e.HandleSirenState(SirenAlertOver)
	if !e.Silence() {
		t.Fatal("Silence() = false")
	}
	expect(t, e, siren, StateArmedAway, "disarm", "arm")
}

func TestSilenceNightMode(t *testing.T) {
	e, siren := newTestEngine(t, silenceCfg)
	e.HandleCommand("arm_night", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	expect(t, e, siren, StateTriggered, "arm night", "alert")
	if !e.Silence() {
		t.Fatal("Silence() = false")
	}
	expect(t, e, siren, StateArmedNight, "disarm", "arm night")
}

func TestSilenceOnlyWhenTriggered(t *testing.T) {
	e, siren := newTestEngine(t, silenceCfg)
	if e.Silence() {
		t.Error("Silence() = true when disarmed")
	}
	expect(t, e, siren, StateDisarmed)

	e.HandleCommand("arm_away", SourceRemote)
	siren.take()
	if e.Silence() {
		t.Error("Silence() = true when armed")
	}
	expect(t, e, siren, StateArmedAway)

	e.HandleSensorEvent(2, "DWS", true)
	siren.take()
	if e.Silence() {
		t.Error("Silence() = true when pending")
	}
	expect(t, e, siren, StatePending)
}

func TestSilenceIgnoresSensorStillOpen(t *testing.T) {
	e, siren := triggered(t)
	e.Silence()
	siren.take()
	e.HandleSensorEvent(1, "DWS", true) // still open, reported again
	expect(t, e, siren, StateArmedAway)
	e.HandleSensorEvent(1, "DWS", false) // closed
	e.HandleSensorEvent(1, "DWS", true)  // opened again
	expect(t, e, siren, StateTriggered, "alert")
}

func TestAfterSilenceDetectionIsInstant(t *testing.T) {
	e, siren := triggered(t)
	e.Silence()
	siren.take()
	e.HandleSensorEvent(2, "DWS", true)          // a delayed door
	expect(t, e, siren, StateTriggered, "alert") // no pending, no "entry delay"
}

func TestSilenceIgnoresLateSirenAlert(t *testing.T) {
	e, siren := triggered(t)
	e.Silence()
	siren.take()
	e.HandleSirenState(SirenAlertOver) // sent before the press, received after
	expect(t, e, siren, StateArmedAway)
	e.HandleSirenState(SirenArmed) // the siren took the re-arming
	e.HandleSirenState(SirenAlert) // a real alert now counts again
	expect(t, e, siren, StateTriggered)
}

func TestDisarmEndsPostSilence(t *testing.T) {
	e, siren := triggered(t)
	e.Silence()
	e.HandleCommand("disarm", SourceLocal)
	e.HandleCommand("arm_away", SourceRemote)
	siren.take()
	e.HandleSensorEvent(2, "DWS", true)
	expect(t, e, siren, StatePending, "entry delay")
}

func TestPostSilencePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarm.json")
	cfg := func(id int) SensorConfig { return silenceCfg[id] }
	siren := &fakeSiren{}
	e := New(path, siren, cfg, nil, nil)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	if !e.Silence() {
		t.Fatal("Silence() = false")
	}
	e.Close()

	siren2 := &fakeSiren{}
	e2 := New(path, siren2, cfg, nil, nil)
	t.Cleanup(e2.Close)
	if err := e2.Load(); err != nil {
		t.Fatal(err)
	}
	siren2.take()
	e2.HandleSensorEvent(2, "DWS", true)
	expect(t, e2, siren2, StateTriggered, "alert")
}
