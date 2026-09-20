package main

import (
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

// TestSensorInAlarmByType pins down which sensor types can put the system in
// an alarm state. A siren is an actuator and must never trigger on its own:
// the removed Tamper field was always false, so relying on it silently made
// the SRN branch dead code that looked functional.
func TestSensorInAlarmByType(t *testing.T) {
	tests := []struct {
		name   string
		sensor camera.Sensor
		want   bool
	}{
		{"DWS open", camera.Sensor{Type: "DWS", Open: true}, true},
		{"DWS closed", camera.Sensor{Type: "DWS", Open: false}, false},
		{"PIR motion", camera.Sensor{Type: "PIR", Motion: true}, true},
		{"PIR idle", camera.Sensor{Type: "PIR", Motion: false}, false},

		// A siren never triggers the alarm, whatever its other fields say.
		{"SRN never triggers", camera.Sensor{Type: "SRN", Open: true, Motion: true}, false},

		// A keypad arms/disarms through its own event path, never through
		// sensorInAlarm.
		{"KPD never triggers", camera.Sensor{Type: "KPD", Open: true, Motion: true}, false},

		{"unknown type", camera.Sensor{Type: "UNKNOWN", Open: true, Motion: true}, false},
		{"empty type", camera.Sensor{Open: true, Motion: true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := sensorInAlarm(tt.sensor); got != tt.want {
				t.Errorf("sensorInAlarm(%+v) = %v, want %v", tt.sensor, got, tt.want)
			}
		})
	}
}
