package alarm

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// alerts records the alerts the hook receives. The entry delay fires from a
// timer goroutine, hence the mutex.
type alerts struct {
	mu  sync.Mutex
	got []Alert
}

func (a *alerts) add(al Alert) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.got = append(a.got, al)
}

func (a *alerts) list() []Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.got)
}

func hooked(t *testing.T) (*Engine, *fakeSiren, *alerts) {
	t.Helper()
	e, siren := newTestEngine(t, silenceCfg)
	a := &alerts{}
	e.SetAlertHook(a.add)
	return e, siren, a
}

func wantAlerts(t *testing.T, a *alerts, want ...Alert) {
	t.Helper()
	if got := a.list(); !slices.Equal(got, want) {
		t.Errorf("alerts = %+v, want %+v", got, want)
	}
}

func TestAlertHookOnInstantTrigger(t *testing.T) {
	e, _, a := hooked(t)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	wantAlerts(t, a, Alert{SensorID: 1})
}

func TestAlertHookOnEntryDelayOver(t *testing.T) {
	e, _, a := hooked(t)
	short(t, e)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(2, "DWS", true)
	wantAlerts(t, a) // entry delay: not set off yet
	time.Sleep(100 * time.Millisecond)
	wantAlerts(t, a, Alert{SensorID: 2})
}

func TestAlertHookOnRedetection(t *testing.T) {
	e, _, a := hooked(t)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	e.HandleSensorEvent(2, "DWS", true)
	wantAlerts(t, a, Alert{SensorID: 1}, Alert{SensorID: 2, Again: true})
}

func TestAlertHookSirenSetOff(t *testing.T) {
	e, _, a := hooked(t)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSirenState(SirenAlert)
	wantAlerts(t, a, Alert{SensorID: 0})
}

func TestAlertHookQuietOnSilenceAndDisarm(t *testing.T) {
	e, _, a := hooked(t)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	if !e.Silence() {
		t.Fatal("Silence() = false")
	}
	e.HandleCommand("disarm", SourceRemote)
	wantAlerts(t, a, Alert{SensorID: 1})
}

func TestAlertHookFreshAfterSilence(t *testing.T) {
	e, _, a := hooked(t)
	e.HandleCommand("arm_away", SourceRemote)
	e.HandleSensorEvent(1, "DWS", true)
	if !e.Silence() {
		t.Fatal("Silence() = false")
	}
	e.HandleSensorEvent(2, "DWS", true)
	wantAlerts(t, a, Alert{SensorID: 1}, Alert{SensorID: 2})
}
