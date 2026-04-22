// Package config manages persistent configuration stored on the camera.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Config is the root configuration persisted to disk.
type Config struct {
	MQTT    MQTTConfig    `json:"mqtt"`
	HomeKit HomeKitConfig `json:"homekit"`
	Admin   AdminConfig   `json:"admin"`
	Web     WebConfig     `json:"web"`
	Alarm   AlarmConfig   `json:"alarm"`
	Sensors    []SensorEntry `json:"sensors,omitempty"`
	DeletedIDs []int         `json:"deleted_ids,omitempty"`
}

// WebConfig holds web UI settings.
type WebConfig struct {
	// Enabled controls whether the HTTP server is started at boot.
	// If false, openqiara runs headless (MQTT/HomeKit/alarm still active).
	// Read only at boot — a restart is required to toggle.
	// Zero value (false) means enabled for backward compatibility with existing
	// configs that don't have this field — handled in the load logic below.
	Enabled *bool `json:"enabled,omitempty"`
}

// AlarmConfig selects the alarm operating mode and related settings.
type AlarmConfig struct {
	// Mode is either "standalone" (default, openqiara is the brain) or
	// "alarmo" (openqiara is a bridge between KPD and Alarmo via MQTT).
	Mode string `json:"mode,omitempty"`

	// AlarmoCommandTopic is the MQTT topic where Alarmo listens for commands
	// (default "alarmo/command"). Used only in mode "alarmo".
	AlarmoCommandTopic string `json:"alarmo_command_topic,omitempty"`

	// AlarmoStateTopic is the MQTT topic where Alarmo publishes its state
	// (default "alarmo/state"). Used only in mode "alarmo".
	AlarmoStateTopic string `json:"alarmo_state_topic,omitempty"`

	// SirenSounds controls which sounds the physical siren plays.
	// "all"  (default) = arm beep + disarm beep + triggered wail
	// "alarm_only"     = only triggered wail (no arm/disarm beeps)
	// "none"           = completely silent (siren disabled)
	SirenSounds string `json:"siren_sounds"`

	// ArmingDelaySeconds is the grace period after ARM_AWAY before surveillance
	// actually starts (default 60s). 0 means "use default".
	ArmingDelaySeconds int `json:"arming_delay_seconds,omitempty"`

	// PendingDelaySeconds is the grace period after a sensor triggers in armed
	// mode before the siren fires (default 60s). 0 means "use default".
	PendingDelaySeconds int `json:"pending_delay_seconds,omitempty"`

	// WailDurationSeconds is how long the siren wails before being stopped
	// during an alarm burst (default 3s). 0 means "use default".
	WailDurationSeconds int `json:"wail_duration_seconds,omitempty"`
}

// Defaults for alarm timings, exposed so other packages don't redeclare them.
const (
	DefaultArmingDelaySeconds  = 60
	DefaultPendingDelaySeconds = 60
	DefaultWailDurationSeconds = 3
)

// ArmingDelay returns the configured arming delay, falling back to default.
func (c Config) ArmingDelay() time.Duration {
	if c.Alarm.ArmingDelaySeconds > 0 {
		return time.Duration(c.Alarm.ArmingDelaySeconds) * time.Second
	}
	return DefaultArmingDelaySeconds * time.Second
}

// PendingDelay returns the configured pending delay, falling back to default.
func (c Config) PendingDelay() time.Duration {
	if c.Alarm.PendingDelaySeconds > 0 {
		return time.Duration(c.Alarm.PendingDelaySeconds) * time.Second
	}
	return DefaultPendingDelaySeconds * time.Second
}

// WailDuration returns the configured wail duration, falling back to default.
func (c Config) WailDuration() time.Duration {
	if c.Alarm.WailDurationSeconds > 0 {
		return time.Duration(c.Alarm.WailDurationSeconds) * time.Second
	}
	return DefaultWailDurationSeconds * time.Second
}

// WebEnabled returns whether the HTTP server should start. Defaults to true
// if the config has never been touched (Enabled is nil).
func (c Config) WebEnabled() bool {
	if c.Web.Enabled == nil {
		return true
	}
	return *c.Web.Enabled
}

// AlarmMode returns the effective alarm mode, defaulting to "standalone".
func (c Config) AlarmMode() string {
	if c.Alarm.Mode == "alarmo" {
		return "alarmo"
	}
	return "standalone"
}

// SirenSoundsMode returns the effective siren sounds mode, defaulting to "all".
func (c Config) SirenSoundsMode() string {
	switch c.Alarm.SirenSounds {
	case "alarm_only", "none":
		return c.Alarm.SirenSounds
	default:
		return "all"
	}
}

// AlarmoTopics returns the command and state topics with defaults applied.
func (c Config) AlarmoTopics() (command, state string) {
	command = c.Alarm.AlarmoCommandTopic
	if command == "" {
		command = "alarmo/command"
	}
	state = c.Alarm.AlarmoStateTopic
	if state == "" {
		state = "alarmo/state"
	}
	return
}

// HomeKitConfig holds HomeKit bridge settings.
type HomeKitConfig struct {
	Enabled bool                `json:"enabled"`
	Pin     string              `json:"pin,omitempty"`
	Name    string              `json:"name,omitempty"`
	Camera  HomeKitCameraConfig `json:"camera,omitempty"`
}

// HomeKitCameraConfig configures the optional HomeKit IP Camera accessory
// exposed by the bridge. Disabled by default until streaming is wired.
type HomeKitCameraConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	Name    string `json:"name,omitempty"`
	HLSPath string `json:"hls_path,omitempty"`
}

// MQTTConfig holds MQTT broker connection settings.
type MQTTConfig struct {
	Broker      string `json:"broker"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	TopicPrefix string `json:"topic_prefix,omitempty"`
}

// AdminConfig holds web UI access settings.
// AdminConfig holds web UI admin credentials.
// If Password is empty, the web UI is accessible without authentication.
// Username is fixed to "admin" (not configurable).
type AdminConfig struct {
	Password string `json:"password,omitempty"`
}

// SensorEntry holds a paired sensor's persistent data.
//
// Day/Night alarm flags mirror fbxhome's ExportLink properties:
// - DayAlarm/NightAlarm: true = sensor triggers the alarm in that mode.
//   Default true (set explicitly at sensor creation).
// - DayTimed/NightTimed: true = sensor honours timeout_before_alert before
//   firing (entry/exit delay). Default true.
// In standalone alarm mode, NightAllowed (legacy) is still consulted; in
// fbxhome bridge mode the new fields are pushed to fbxhome.
type SensorEntry struct {
	ID           int    `json:"id"`
	Type         string `json:"type"`
	Model        string `json:"model"`
	Label        string `json:"label,omitempty"`
	KPDCode      string `json:"kpd_code,omitempty"`
	KPDCodeLabel string `json:"kpd_code_label,omitempty"`
	NightAllowed bool   `json:"night_allowed,omitempty"` // legacy: standalone mode only

	// fbxhome ExportLink-backed flags. *bool so we can detect "unset" vs
	// "explicitly false". Unset = inherit fbxhome default (true).
	DayAlarm   *bool `json:"day_alarm,omitempty"`
	NightAlarm *bool `json:"night_alarm,omitempty"`
	DayTimed   *bool `json:"day_timed,omitempty"`
	NightTimed *bool `json:"night_timed,omitempty"`
}

// Store loads and saves configuration from a JSON file.
type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

// NewStore creates a config store backed by the given file path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Load reads the configuration from disk. If the file does not exist,
// a default configuration is used.
func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.cfg = Config{
			MQTT: MQTTConfig{
				TopicPrefix: "openqiara",
			},
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	if err := json.Unmarshal(data, &s.cfg); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

// Save writes the current configuration to disk.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Get returns a copy of the current configuration.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update applies a mutation function to the configuration and saves it.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	fn(&s.cfg)
	s.mu.Unlock()
	return s.Save()
}
