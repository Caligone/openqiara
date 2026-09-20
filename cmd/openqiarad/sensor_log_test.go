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
			sensor:     camera.Sensor{Type: "PIR", Open: true, Motion: true, Battery: 80},
			wantField:  "motion",
			avoidField: "open",
		},
		{
			name:       "DWS only logs open",
			sensor:     camera.Sensor{Type: "DWS", Open: true, Motion: true, Battery: 70},
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
