package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/config"
)

// fakeCam capture les appels SRN pour assertions.
type fakeCam struct {
	sensors []camera.Sensor

	triggerSirenCalls      []int
	triggerSirenAlarmCalls []triggerAlarmCall
	stopSirenCalls         []int

	// erreurs simulées
	triggerSirenErr      error
	triggerSirenAlarmErr error
	stopSirenErr         error
}

type triggerAlarmCall struct {
	id       int
	duration time.Duration
}

func (f *fakeCam) CachedSensors() []camera.Sensor { return f.sensors }

func (f *fakeCam) TriggerSiren(_ context.Context, id int) error {
	f.triggerSirenCalls = append(f.triggerSirenCalls, id)
	return f.triggerSirenErr
}

func (f *fakeCam) TriggerSirenAlarm(_ context.Context, id int, d time.Duration) error {
	f.triggerSirenAlarmCalls = append(f.triggerSirenAlarmCalls, triggerAlarmCall{id, d})
	return f.triggerSirenAlarmErr
}

func (f *fakeCam) StopSiren(_ context.Context, id int) error {
	f.stopSirenCalls = append(f.stopSirenCalls, id)
	return f.stopSirenErr
}

// helper : construit un controller synchrone pour tests inline.
func newTestController(t *testing.T, cam sirenAPI, sirenSounds string) (*sirenController, *config.Store) {
	t.Helper()
	store := config.NewStore(t.TempDir() + "/config.json")
	if sirenSounds != "" {
		if err := store.Update(func(c *config.Config) {
			c.Alarm.SirenSounds = sirenSounds
		}); err != nil {
			t.Fatalf("Update store: %v", err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sc := newSirenController(context.Background(), cam, store, logger)
	sc.synchronous = true
	return sc, store
}

func srnSensor(id int) camera.Sensor {
	return camera.Sensor{ID: id, Type: "SRN", Reachable: true}
}

// TestSirenReadyBootSkipsDisarmed : au boot, le premier callback "disarmed"
// est ignoré (sirenReady=false → skip).
func TestSirenReadyBootSkipsDisarmed(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "all")

	sc.Handle("disarmed", "")

	if len(cam.stopSirenCalls) != 0 {
		t.Errorf("expected no StopSiren call at boot, got %d", len(cam.stopSirenCalls))
	}
}

// TestNoTransitionSkipsAll : newState == prevState → rien.
func TestNoTransitionSkipsAll(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "all")
	sc.sirenReady = true // skip boot guard

	sc.Handle("armed_away", "armed_away")

	if len(cam.triggerSirenCalls)+len(cam.triggerSirenAlarmCalls)+len(cam.stopSirenCalls) != 0 {
		t.Errorf("expected no calls on identity transition")
	}
}

// TestArmingBeepFbxhome : "arming" en mode fbxhome (pas charmux) → TriggerSiren.
func TestArmingBeepFbxhome(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "all")
	sc.sirenReady = true

	sc.Handle("arming", "disarmed")

	if len(cam.triggerSirenCalls) != 1 || cam.triggerSirenCalls[0] != 32 {
		t.Errorf("expected TriggerSiren(32), got %v", cam.triggerSirenCalls)
	}
	if len(cam.triggerSirenAlarmCalls) != 0 {
		t.Errorf("unexpected TriggerSirenAlarm calls: %v", cam.triggerSirenAlarmCalls)
	}
}

// TestArmingNoBeepWhenSirenSoundsAlarmOnly : "arming" avec siren_sounds=alarm_only → silence.
func TestArmingNoBeepWhenSirenSoundsAlarmOnly(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "alarm_only")
	sc.sirenReady = true

	sc.Handle("arming", "disarmed")

	if len(cam.triggerSirenCalls) != 0 {
		t.Errorf("alarm_only mode should not beep on arming, got %v", cam.triggerSirenCalls)
	}
}

// TestTriggeredFiresWail : "triggered" → TriggerSirenAlarm avec wail duration.
func TestTriggeredFiresWail(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, store := newTestController(t, cam, "all")
	sc.sirenReady = true
	_ = store.Update(func(c *config.Config) { c.Alarm.WailDurationSeconds = 15 })

	sc.Handle("triggered", "pending")

	if len(cam.triggerSirenAlarmCalls) != 1 {
		t.Fatalf("expected 1 TriggerSirenAlarm call, got %d", len(cam.triggerSirenAlarmCalls))
	}
	call := cam.triggerSirenAlarmCalls[0]
	if call.id != 32 {
		t.Errorf("expected addr=32, got %d", call.id)
	}
	if call.duration != 15*time.Second {
		t.Errorf("expected duration=15s, got %v", call.duration)
	}
}

// TestTriggeredFiresEvenInAlarmOnlyMode : siren_sounds=alarm_only laisse passer le wail.
func TestTriggeredFiresEvenInAlarmOnlyMode(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "alarm_only")
	sc.sirenReady = true

	sc.Handle("triggered", "pending")

	if len(cam.triggerSirenAlarmCalls) != 1 {
		t.Errorf("alarm_only mode MUST still fire wail on triggered, got %d calls", len(cam.triggerSirenAlarmCalls))
	}
}

// TestDisarmedStopsSiren : disarmed → StopSiren.
func TestDisarmedStopsSiren(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "all")
	sc.sirenReady = true

	sc.Handle("disarmed", "triggered")

	if len(cam.stopSirenCalls) != 1 || cam.stopSirenCalls[0] != 32 {
		t.Errorf("expected StopSiren(32), got %v", cam.stopSirenCalls)
	}
	// En mode "all" + fbxhome (pas charmux), on émet aussi un beep de disarm via TriggerSiren.
	if len(cam.triggerSirenCalls) != 1 {
		t.Errorf("expected disarm beep via TriggerSiren in 'all' mode, got %d", len(cam.triggerSirenCalls))
	}
}

// TestNoneModeAlwaysStops : siren_sounds=none → StopSiren peu importe la transition.
func TestNoneModeAlwaysStops(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{srnSensor(32)}}
	sc, _ := newTestController(t, cam, "none")
	sc.sirenReady = true

	sc.Handle("triggered", "pending")

	if len(cam.stopSirenCalls) != 1 {
		t.Errorf("none mode should cut wail on triggered, got %d StopSiren", len(cam.stopSirenCalls))
	}
	if len(cam.triggerSirenAlarmCalls) != 0 {
		t.Errorf("none mode must NOT fire wail, got %v", cam.triggerSirenAlarmCalls)
	}
}

// TestNoSRNNoOp : aucun SRN dans CachedSensors → pas d'appel SRN.
func TestNoSRNNoOp(t *testing.T) {
	cam := &fakeCam{sensors: []camera.Sensor{
		{ID: 14, Type: "DWS"},
		{ID: 29, Type: "KPD"},
	}}
	sc, _ := newTestController(t, cam, "all")
	sc.sirenReady = true

	sc.Handle("triggered", "pending")
	sc.Handle("disarmed", "triggered")

	total := len(cam.triggerSirenCalls) + len(cam.triggerSirenAlarmCalls) + len(cam.stopSirenCalls)
	if total != 0 {
		t.Errorf("no SRN paired → expected 0 calls, got %d total", total)
	}
}

// TestWailErrorDoesntPanic : si TriggerSirenAlarm renvoie une erreur, on log
// mais on ne panique pas.
func TestWailErrorDoesntPanic(t *testing.T) {
	cam := &fakeCam{
		sensors:              []camera.Sensor{srnSensor(32)},
		triggerSirenAlarmErr: errors.New("boom"),
	}
	sc, _ := newTestController(t, cam, "all")
	sc.sirenReady = true

	// Ne panique pas — c'est le test (synchronous=true).
	sc.Handle("triggered", "pending")

	if len(cam.triggerSirenAlarmCalls) != 1 {
		t.Errorf("expected the call to happen even with error")
	}
}
