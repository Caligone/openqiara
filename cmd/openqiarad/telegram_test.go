package main

import (
	"testing"

	"github.com/caligone/openqiara/internal/config"
)

func TestSensorName(t *testing.T) {
	sensors := []config.SensorEntry{
		{ID: 1, Type: "DWS", Label: "Porte d'entrée"},
		{ID: 3, Type: "DWS"},
		{ID: 5, Type: "PIR"},
	}
	cases := []struct {
		id   int
		want string
	}{
		{1, "Porte d'entrée"},
		{3, "Ouverture 3"},
		{5, "Mouvement 5"},
		{9, "Capteur 9"},
	}
	for _, c := range cases {
		if got := sensorName(sensors, c.id); got != c.want {
			t.Errorf("sensorName(%d) = %q, want %q", c.id, got, c.want)
		}
	}
}
