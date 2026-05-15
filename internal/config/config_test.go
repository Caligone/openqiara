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

func TestHashAdminPassword(t *testing.T) {
	hash, err := HashAdminPassword("hunter2")
	if err != nil {
		t.Fatalf("HashAdminPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("hash is empty")
	}
	if hash == "hunter2" {
		t.Fatal("hash equals plaintext")
	}

	ac := AdminConfig{PasswordHash: hash}
	if !ac.CheckPassword("hunter2") {
		t.Error("CheckPassword should accept the correct password")
	}
	if ac.CheckPassword("hunter3") {
		t.Error("CheckPassword should reject a wrong password")
	}
	if ac.CheckPassword("") {
		t.Error("CheckPassword should reject empty")
	}

	// Empty plaintext → empty hash, AuthEnabled=false.
	emptyHash, _ := HashAdminPassword("")
	if emptyHash != "" {
		t.Errorf("HashAdminPassword(\"\") = %q, want empty", emptyHash)
	}
	if (AdminConfig{}).AuthEnabled() {
		t.Error("empty config should not have auth enabled")
	}
}

func TestAdminConfigLegacyFallback(t *testing.T) {
	// Config legacy : Password en clair, pas de hash. CheckPassword doit
	// fonctionner en attendant la migration.
	ac := AdminConfig{Password: "legacy123"}
	if !ac.AuthEnabled() {
		t.Error("legacy config should report auth enabled")
	}
	if !ac.CheckPassword("legacy123") {
		t.Error("legacy CheckPassword should accept the clear-text match")
	}
	if ac.CheckPassword("wrong") {
		t.Error("legacy CheckPassword should reject wrong")
	}
}

func TestStoreLoadMigratesLegacyPassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// Écrit un config legacy avec password en clair.
	data := `{"admin":{"password":"legacy"}}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}

	s := NewStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	cfg := s.Get()
	if cfg.Admin.Password != "" {
		t.Errorf("legacy Password field still set after migration: %q", cfg.Admin.Password)
	}
	if cfg.Admin.PasswordHash == "" {
		t.Error("PasswordHash should be set after migration")
	}
	if !cfg.Admin.CheckPassword("legacy") {
		t.Error("migrated config should accept the original password")
	}
}
