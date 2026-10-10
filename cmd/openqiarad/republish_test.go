package main

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

// TestRepublishSensorStatesKeepsUnreportedState: on an MQTT (re)connect, a
// door or motion sensor not heard since the start is left out, so that its
// retained state, open included, is not overwritten as closed.
func TestRepublishSensorStatesKeepsUnreportedState(t *testing.T) {
	sensors := []camera.Sensor{
		{ID: 46, Type: "DWS"}, // open before the restart, silent since
		{ID: 52, Type: "DWS", Open: true, Reported: true},
		{ID: 60, Type: "PIR"},
		{ID: 29, Type: "SRN", SirenState: "armed"},
		{ID: 31, Type: "KPD"},
	}
	var published []int
	republishSensorStates(context.Background(), sensors, func(_ context.Context, s camera.Sensor) error {
		published = append(published, s.ID)
		return nil
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if want := []int{52, 29}; !slices.Equal(published, want) {
		t.Errorf("published %v, want %v", published, want)
	}
}
