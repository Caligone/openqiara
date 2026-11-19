package alarm

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// fakeSiren records what the engine asks of the siren.
type fakeSiren struct {
	absent bool
	calls  []string
}

func (f *fakeSiren) Present() bool { return !f.absent }
func (f *fakeSiren) Arm(night, delayed bool) {
	call := "arm"
	if night {
		call += " night"
	}
	if delayed {
		call += " delayed"
	}
	f.calls = append(f.calls, call)
}
func (f *fakeSiren) Disarm()     { f.calls = append(f.calls, "disarm") }
func (f *fakeSiren) EntryDelay() { f.calls = append(f.calls, "entry delay") }
func (f *fakeSiren) Alert()      { f.calls = append(f.calls, "alert") }

// take returns the calls since the last take.
func (f *fakeSiren) take() []string {
	c := f.calls
	f.calls = nil
	return c
}

func newTestEngine(t *testing.T, cfg map[int]SensorConfig) (*Engine, *fakeSiren) {
	t.Helper()
	siren := &fakeSiren{}
	e := New(filepath.Join(t.TempDir(), "alarm.json"), siren, func(id int) SensorConfig { return cfg[id] }, nil, nil)
	t.Cleanup(e.Close) // before t.TempDir's: a late timer would write in it
	return e, siren
}

func expect(t *testing.T, e *Engine, siren *fakeSiren, state State, calls ...string) {
	t.Helper()
	if s := e.Snapshot().State; s != state {
		t.Errorf("state = %q, want %q", s, state)
	}
	if got := siren.take(); !slices.Equal(got, calls) {
		t.Errorf("siren calls = %q, want %q", got, calls)
	}
}

// short makes the delays and the siren's margin short for a test.
func short(t *testing.T, e *Engine) {
	t.Helper()
	old := sirenMargin
	t.Cleanup(func() { sirenMargin = old })
	sirenMargin = 10 * time.Millisecond
	e.SetTimings(20*time.Millisecond, 20*time.Millisecond)
}

// TestKeypadCycle: the keypad arms with the exit delay; a door starts the
// entry delay, the siren's reports move the alarm on, and a disarm ends it.
func TestKeypadCycle(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	expect(t, e, siren, StateArming, "arm delayed")
	if e.Snapshot().TimerRemaining == 0 {
		t.Error("no exit delay shown")
	}
	e.HandleSirenState(SirenExitDelay)
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StateArmedAway)

	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StatePending, "entry delay")
	e.HandleSirenState(SirenEntryDelay)
	e.HandleSirenState(SirenAlert)
	expect(t, e, siren, StateTriggered)
	e.HandleSirenState(SirenAlertOver)
	e.HandleSensorEvent(17, "PIR", true)
	expect(t, e, siren, StateTriggered, "alert")
	if by := e.Snapshot().TriggeredBy; by != 23 {
		t.Errorf("triggered by %d, want 23", by)
	}

	e.HandleCommand("disarm", SourceLocal)
	expect(t, e, siren, StateDisarmed, "disarm")
}

// TestNeverBack: a late or stray report, or a siren that rebooted, never
// moves the alarm back; set off, it leaves only when disarmed.
func TestNeverBack(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	siren.take()
	for _, s := range []SirenState{SirenArmed, SirenOff, SirenExitDelay} {
		e.HandleSirenState(s)
	}
	expect(t, e, siren, StatePending)
	e.HandleSirenState(SirenAlert)
	for _, s := range []SirenState{SirenArmed, SirenOff, SirenEntryDelay} {
		e.HandleSirenState(s)
	}
	expect(t, e, siren, StateTriggered)
}

// TestDelaysWithoutReport: a delay the siren does not report the end of
// (torn off, a report lost) ends all the same, a little later; the end of
// an entry delay sets the alarm off and the siren too.
func TestDelaysWithoutReport(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	short(t, e)
	e.HandleCommand("arm_away", SourceLocal)
	time.Sleep(80 * time.Millisecond)
	expect(t, e, siren, StateArmedAway, "arm delayed")

	e.HandleSensorEvent(23, "DWS", true)
	time.Sleep(80 * time.Millisecond)
	expect(t, e, siren, StateTriggered, "entry delay", "alert")
}

// TestDisarmStopsTheDelay: a disarm cancels the entry delay's end.
func TestDisarmStopsTheDelay(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	short(t, e)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleCommand("disarm", SourceLocal)
	time.Sleep(80 * time.Millisecond)
	expect(t, e, siren, StateDisarmed, "arm", "entry delay", "disarm")
}

// TestRemoteArmIsImmediate: Home Assistant and HomeKit arm with no exit
// delay, and a sensor already in alarm is taken once armed.
func TestRemoteArmIsImmediate(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleCommand("arm_night", SourceRemote)
	expect(t, e, siren, StatePending, "arm night", "entry delay")
}

// TestInstantSensor: an instant sensor sets the alarm off at once, armed
// or during the entry delay.
func TestInstantSensor(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{30: {Instant: true}})
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(30, "DWS", true)
	expect(t, e, siren, StateTriggered, "arm", "alert")

	e.HandleCommand("disarm", SourceLocal)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StatePending, "disarm", "arm", "entry delay")
	e.HandleSensorEvent(30, "DWS", false)
	e.HandleSensorEvent(30, "DWS", true)
	expect(t, e, siren, StateTriggered, "alert")
}

// TestWhatIsIgnored: the keypad, a sensor back to rest, anything while
// disarmed or arming, and a night-allowed sensor when armed for the night.
// A change of mode arms the siren again for it.
func TestWhatIsIgnored(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{17: {NightAllowed: true}})
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleSensorEvent(23, "DWS", false)
	expect(t, e, siren, StateDisarmed)

	e.HandleCommand("arm_night", SourceLocal)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleSensorEvent(23, "DWS", false)
	expect(t, e, siren, StateArming, "arm night delayed")
	e.HandleSirenState(SirenArmed)
	e.HandleSensorEvent(14, "KPD", true)
	e.HandleSensorEvent(17, "PIR", true)
	expect(t, e, siren, StateArmedNight)

	e.HandleSensorEvent(17, "PIR", false)
	e.HandleCommand("arm_away", SourceRemote)
	expect(t, e, siren, StateArmedAway, "arm")
	e.HandleSensorEvent(17, "PIR", true)
	expect(t, e, siren, StatePending, "entry delay")
}

// TestModeSwitchSeesSensorsInAlarm: from night to away, a door open that
// night left out now starts the entry delay.
func TestModeSwitchSeesSensorsInAlarm(t *testing.T) {
	e, siren := newTestEngine(t, map[int]SensorConfig{23: {NightAllowed: true}})
	e.HandleCommand("arm_night", SourceRemote)
	e.HandleSensorEvent(23, "DWS", true)
	expect(t, e, siren, StateArmedNight, "arm night")
	e.HandleCommand("arm_away", SourceRemote)
	expect(t, e, siren, StatePending, "arm", "entry delay")
}

// TestExitPastMotionSensor: leaving walks past a PIR during the exit
// delay; its motion, still reported when the delay ends, sets nothing off.
func TestExitPastMotionSensor(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.HandleSensorEvent(17, "PIR", true)
	e.HandleSensorEvent(23, "DWS", true)
	e.HandleSensorEvent(23, "DWS", false)
	siren.take()
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StateArmedAway)
}

// TestExitDoorLeftOpen: a door still open when the exit delay ends starts
// the entry delay.
func TestExitDoorLeftOpen(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.HandleSensorEvent(23, "DWS", true)
	siren.take()
	e.HandleSirenState(SirenArmed)
	expect(t, e, siren, StatePending, "entry delay")
}

// TestLoadUnreadable: a state file that cannot be read starts disarmed,
// and so is the siren.
func TestLoadUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarm.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	siren := &fakeSiren{}
	e := New(path, siren, nil, nil, nil)
	if err := e.Load(); err == nil {
		t.Fatal("a broken file loaded")
	}
	expect(t, e, siren, StateDisarmed, "disarm")
}

// TestNoSiren: without a siren (or with siren_sounds none), the engine's
// delays are the alarm's.
func TestNoSiren(t *testing.T) {
	e, siren := newTestEngine(t, nil)
	siren.absent = true
	e.SetTimings(20*time.Millisecond, 20*time.Millisecond)
	e.HandleCommand("arm_away", SourceLocal)
	time.Sleep(50 * time.Millisecond)
	expect(t, e, siren, StateArmedAway, "arm delayed")
	e.HandleSensorEvent(23, "DWS", true)
	time.Sleep(50 * time.Millisecond)
	expect(t, e, siren, StateTriggered, "entry delay", "alert")
}

// TestLoad: an exit delay cut by a restart ends armed, an entry delay
// starts again; the siren is told what to want. Files from before keep
// their mode.
func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		file  string
		state State
		mode  State
		calls []string
	}{
		{`{"state":"arming","mode":"armed_night"}`, StateArmedNight, StateArmedNight, []string{"arm night"}},
		{`{"state":"pending","previous_state":"armed_night"}`, StatePending, StateArmedNight, []string{"arm night"}},
		{`{"state":"triggered","mode":"armed_away","triggered_by":23}`, StateTriggered, StateArmedAway, []string{"arm"}},
		{`{"state":"bogus"}`, StateDisarmed, "", []string{"disarm"}},
	} {
		path := filepath.Join(t.TempDir(), "alarm.json")
		if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
			t.Fatal(err)
		}
		siren := &fakeSiren{}
		e := New(path, siren, nil, nil, nil)
		if err := e.Load(); err != nil {
			t.Fatal(err)
		}
		if s := e.Snapshot(); s.State != tc.state || s.PreviousState != tc.mode {
			t.Errorf("%s: loaded %+v, want %s/%s", tc.file, s, tc.state, tc.mode)
		}
		if got := siren.take(); !slices.Equal(got, tc.calls) {
			t.Errorf("%s: siren calls = %q, want %q", tc.file, got, tc.calls)
		}
	}
}

// TestStatePersisted: what is saved loads back.
func TestStatePersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarm.json")
	e := New(path, &fakeSiren{}, nil, nil, nil)
	e.HandleCommand("arm_night", SourceRemote)
	again := New(path, &fakeSiren{}, nil, nil, nil)
	if err := again.Load(); err != nil {
		t.Fatal(err)
	}
	if s := again.Snapshot(); s.State != StateArmedNight || s.ArmedAt == 0 {
		t.Errorf("loaded %+v, want armed_night with its arming time", s)
	}
}

// TestCallbackFiresOnTransition: every change of state is announced.
func TestCallbackFiresOnTransition(t *testing.T) {
	var got []State
	e := New(filepath.Join(t.TempDir(), "alarm.json"), &fakeSiren{}, nil,
		func(s Snapshot) { got = append(got, s.State) }, nil)
	e.HandleCommand("arm_away", SourceLocal)
	e.HandleSirenState(SirenArmed)
	e.HandleCommand("disarm", SourceLocal)
	if want := []State{StateArming, StateArmedAway, StateDisarmed}; !slices.Equal(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}
