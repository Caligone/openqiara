package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreLoadDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "config.json"))

	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	cfg := s.Get()
	if cfg.MQTT.TopicPrefix != "openqiara" {
		t.Errorf("TopicPrefix = %q, want openqiara", cfg.MQTT.TopicPrefix)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := NewStore(path)

	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	err := s.Update(func(c *Config) {
		c.MQTT.Broker = "tcp://10.0.0.1:1883"
		c.Sensors = append(c.Sensors, SensorEntry{
			ID: 33, Type: "DWS", Model: "HOMELABDWS00ACFD", Label: "Porte entrée",
		})
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	cfg := s2.Get()
	if cfg.MQTT.Broker != "tcp://10.0.0.1:1883" {
		t.Errorf("Broker = %q", cfg.MQTT.Broker)
	}
	if len(cfg.Sensors) != 1 || cfg.Sensors[0].ID != 33 {
		t.Errorf("Sensors = %+v", cfg.Sensors)
	}
}

func TestStoreLoadExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	data := `{"mqtt":{"broker":"tcp://test:1883","topic_prefix":"custom"}}`
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	cfg := s.Get()
	if cfg.MQTT.Broker != "tcp://test:1883" {
		t.Errorf("Broker = %q", cfg.MQTT.Broker)
	}
	if cfg.MQTT.TopicPrefix != "custom" {
		t.Errorf("TopicPrefix = %q", cfg.MQTT.TopicPrefix)
	}
}
