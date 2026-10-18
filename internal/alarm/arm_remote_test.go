package alarm

import (
	"slices"
	"testing"
)

func TestArmRemoteFromDisarmed(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	if r := e.ArmRemote(false, false); r != ArmDone {
		t.Errorf("result = %q, want %q", r, ArmDone)
	}
	expect(t, e, siren, StateArmedAway, "arm")
	if e.Snapshot().ArmedAt == 0 {
		t.Error("armed_at not set")
	}
}

func TestArmRemoteNight(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	if r := e.ArmRemote(true, false); r != ArmDone {
		t.Errorf("result = %q, want %q", r, ArmDone)
	}
	expect(t, e, siren, StateArmedNight, "arm night")
}

func TestArmRemoteOpenSetsOff(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{1: {Instant: true}})
	e.HandleSensorEvent(1, "DWS", true)
	e.ArmRemote(false, false)
	expect(t, e, siren, StateTriggered, "arm", "alert")
}

func TestArmRemoteIgnoreOpen(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{1: {Instant: true}})
	var alerts int
	e.SetAlertHook(func(Alert) { alerts++ })
	e.HandleSensorEvent(1, "DWS", true)
	if r := e.ArmRemote(false, true); r != ArmDone {
		t.Errorf("result = %q, want %q", r, ArmDone)
	}
	expect(t, e, siren, StateArmedAway, "arm")
	e.HandleSensorEvent(1, "DWS", true)
	expect(t, e, siren, StateArmedAway)
	// A delayed sensor: normal entry delay, no post-silence.
	e.HandleSensorEvent(2, "DWS", true)
	expect(t, e, siren, StatePending, "entry delay")
	e.HandleSensorEvent(2, "DWS", false)
	if alerts != 0 {
		t.Errorf("alerts = %d, want 0", alerts)
	}
	e.HandleSensorEvent(1, "DWS", false)
	e.HandleSensorEvent(1, "DWS", true)
	expect(t, e, siren, StateTriggered, "alert")
	if alerts != 1 {
		t.Errorf("alerts = %d, want 1", alerts)
	}
}

func TestArmRemoteIgnoreOpenDelayedSensor(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{1: {Instant: true}})
	e.HandleSensorEvent(1, "DWS", true)
	e.ArmRemote(false, true)
	siren.take()
	e.HandleSensorEvent(2, "DWS", true)
	expect(t, e, siren, StatePending, "entry delay")
}

func TestArmRemoteAlreadyAndBusy(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.ArmRemote(false, false)
	siren.take()
	if r := e.ArmRemote(false, false); r != ArmAlready {
		t.Errorf("result = %q, want %q", r, ArmAlready)
	}
	expect(t, e, siren, StateArmedAway)

	e.HandleSensorEvent(5, "DWS", true)
	siren.take()
	if r := e.ArmRemote(true, false); r != ArmBusy {
		t.Errorf("pending: result = %q, want %q", r, ArmBusy)
	}
	expect(t, e, siren, StatePending)

	e2, siren2 := newTestEngine(t, nil)
	e2.HandleCommand("arm_away", SourceLocal)
	siren2.take()
	if r := e2.ArmRemote(false, false); r != ArmBusy {
		t.Errorf("arming: result = %q, want %q", r, ArmBusy)
	}
	expect(t, e2, siren2, StateArming)
}

func TestArmRemoteBusyTriggered(t *testing.T) {
	e, siren := triggered(t)
	siren.take()
	if r := e.ArmRemote(false, false); r != ArmBusy {
		t.Errorf("result = %q, want %q", r, ArmBusy)
	}
	expect(t, e, siren, StateTriggered)
}

func TestArmRemoteSwitch(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.ArmRemote(true, false)
	siren.take()
	if r := e.ArmRemote(false, false); r != ArmSwitched {
		t.Errorf("result = %q, want %q", r, ArmSwitched)
	}
	expect(t, e, siren, StateArmedAway, "arm")
}

func TestArmRemoteSwitchIgnoreOpen(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{1: {Instant: true, NightAllowed: true}})
	e.ArmRemote(true, false)
	e.HandleSensorEvent(1, "DWS", true) // not watched at night
	siren.take()
	if r := e.ArmRemote(false, true); r != ArmSwitched {
		t.Errorf("result = %q, want %q", r, ArmSwitched)
	}
	expect(t, e, siren, StateArmedAway, "arm")
}

func TestArmRemoteSwitchOpenSetsOff(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{1: {Instant: true, NightAllowed: true}})
	e.ArmRemote(true, false)
	e.HandleSensorEvent(1, "DWS", true)
	siren.take()
	e.ArmRemote(false, false)
	expect(t, e, siren, StateTriggered, "arm", "alert")
}

func TestInAlarm(t *testing.T) {
	e, _ := newTestEngine(t, map[int]SensorConfig{2: {NightAllowed: true}})
	e.HandleSensorEvent(2, "DWS", true)
	e.HandleSensorEvent(1, "DWS", true)
	e.HandleSensorEvent(3, "DWS", true)
	e.HandleSensorEvent(3, "DWS", false)
	e.HandleSensorEvent(9, "KPD", true)
	if got := e.InAlarm(false); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("InAlarm(false) = %v, want [1 2]", got)
	}
	if got := e.InAlarm(true); !slices.Equal(got, []int{1}) {
		t.Errorf("InAlarm(true) = %v, want [1]", got)
	}
}

func TestDisarmClearsIgnoredFromArm(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{1: {Instant: true}})
	e.HandleSensorEvent(1, "DWS", true)
	e.ArmRemote(false, true)
	e.HandleCommand("disarm", SourceRemote)
	siren.take()
	e.HandleSensorEvent(1, "DWS", true)
	e.ArmRemote(false, false)
	expect(t, e, siren, StateTriggered, "arm", "alert")
}
