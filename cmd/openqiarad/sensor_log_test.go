package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

func TestLogSensorEventUsesTypeSpecificState(t *testing.T) {
	tests := []struct {
		name       string
		sensor     camera.Sensor
		wantField  string
		avoidField string
	}{
		{
			name:       "PIR only logs motion",
			sensor:     camera.Sensor{Type: "PIR", Open: new(true), Motion: new(true), Battery: 80},
			wantField:  "motion",
			avoidField: "open",
		},
		{
			name:       "DWS only logs open",
			sensor:     camera.Sensor{Type: "DWS", Open: new(true), Motion: new(true), Battery: 70},
			wantField:  "open",
			avoidField: "motion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			logSensorEvent(logger, camera.SensorEvent{SensorID: 14, Sensor: tt.sensor}, "test")

			var entry map[string]any
			if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
				t.Fatalf("decode log entry: %v", err)
			}
			if _, ok := entry[tt.wantField]; !ok {
				t.Errorf("missing %q field in %s", tt.wantField, output.String())
			}
			if _, ok := entry[tt.avoidField]; ok {
				t.Errorf("unexpected %q field in %s", tt.avoidField, output.String())
			}
		})
	}
}

// TestLogSensorEventUnknownState: an unknown state logs as such, not as a
// pointer address or as closed.
func TestLogSensorEventUnknownState(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logSensorEvent(logger, camera.SensorEvent{SensorID: 14, Sensor: camera.Sensor{Type: "DWS"}}, "test")
	if !bytes.Contains(output.Bytes(), []byte("open=unknown")) {
		t.Errorf("log = %s, want open=unknown", output.String())
	}
	output.Reset()
	logSensorEvent(logger, camera.SensorEvent{SensorID: 14, Sensor: camera.Sensor{Type: "DWS", Open: new(true)}}, "test")
	if !bytes.Contains(output.Bytes(), []byte("open=true")) {
		t.Errorf("log = %s, want open=true", output.String())
	}
}
