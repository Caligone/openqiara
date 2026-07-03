// Package config manages persistent configuration stored on the camera.
package config

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// adminBcryptCost = cost bcrypt pour le hash du password admin. 10 reste
// sous les ~100ms sur ARMv7 (cam Qiara) et est largement suffisant pour
// un usage LAN-trusted. À augmenter si on industrialise.
const adminBcryptCost = 10

// Config is the root configuration persisted to disk.
type Config struct {
	MQTT       MQTTConfig    `json:"mqtt"`
	HomeKit    HomeKitConfig `json:"homekit"`
	RTSP       RTSPConfig    `json:"rtsp,omitempty"`
	Admin      AdminConfig   `json:"admin"`
	Web        WebConfig     `json:"web"`
	Alarm      AlarmConfig   `json:"alarm"`
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

// RTSPConfig configures the optional standard RTSP server, which exposes
// the camera's H.264 stream (video only, no AAC) for consumers like
// Scrypted, Frigate or VLC. Disabled by default.
type RTSPConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Listen is the bind address, default ":8554".
	Listen string `json:"listen,omitempty"`
	// Path is the stream path, default "openqiara".
	Path string `json:"path,omitempty"`
	// HLSPath overrides the source HLS playlist; defaults to the same
	// path the HomeKit camera uses.
	HLSPath string `json:"hls_path,omitempty"`
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

// AdminConfig holds web UI admin credentials.
// Si PasswordHash et Password sont tous les deux vides, l'UI est ouverte.
// Username fixe = "admin" (non configurable).
//
// Password (clair) est conservé uniquement pour la migration : si on
// trouve un config legacy au boot avec Password set et PasswordHash vide,
// on hash et on vide Password. Une fois la migration faite, ce champ ne
// devrait jamais être réécrit.
type AdminConfig struct {
	// PasswordHash est le hash bcrypt du mot de passe. Vide = pas d'auth.
	PasswordHash string `json:"password_hash,omitempty"`

	// Password (deprecated) est l'ancien champ en clair. Migré au boot vers
	// PasswordHash puis effacé. Ne JAMAIS écrire ce champ depuis le code
	// applicatif — passer par SetAdminPassword.
	Password string `json:"password,omitempty"`
}

// AuthEnabled retourne true ssi un password est configuré (hash ou legacy
// clair en attendant la migration).
func (a AdminConfig) AuthEnabled() bool {
	return a.PasswordHash != "" || a.Password != ""
}

// CheckPassword vérifie un password candidat contre le hash configuré.
// Utilise bcrypt.CompareHashAndPassword qui est constant-time.
//
// Fallback transitoire : si PasswordHash est vide mais Password (clair)
// est set, on compare en clair via subtle. Ce chemin n'est emprunté
// qu'entre le chargement du config legacy et la migration au prochain
// Save() — cf. migrateAdminPassword.
func (a AdminConfig) CheckPassword(candidate string) bool {
	if a.PasswordHash != "" {
		return bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(candidate)) == nil
	}
	if a.Password != "" {
		// Window très étroit (entre Load et premier Save migré).
		// subtle.ConstantTimeCompare retourne 0 si longueurs différentes
		// sans short-circuit, OK pour notre cas.
		return subtle.ConstantTimeCompare([]byte(a.Password), []byte(candidate)) == 1
	}
	return false
}

// HashAdminPassword génère un hash bcrypt pour le password donné. Le
// caller doit ensuite l'affecter à AdminConfig.PasswordHash et vider
// AdminConfig.Password.
func HashAdminPassword(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), adminBcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash admin password: %w", err)
	}
	return string(h), nil
}

// SensorEntry holds a paired sensor's persistent data.
//
// Day/Night alarm flags mirror fbxhome's ExportLink properties:
//   - DayAlarm/NightAlarm: true = sensor triggers the alarm in that mode.
//     Default true (set explicitly at sensor creation).
//   - DayTimed/NightTimed: true = sensor honours timeout_before_alert before
//     firing (entry/exit delay). Default true.
//
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

	// Migration legacy : si on a un password en clair (ancien schéma)
	// et pas de hash, on hash maintenant. Le Save() est différé jusqu'à
	// la prochaine mutation pour éviter d'écrire au boot sur disque ;
	// l'admin actuel reste opérationnel via le fallback CheckPassword.
	// Le boot suivant après n'importe quelle Update() persistera la
	// migration définitivement.
	if s.cfg.Admin.Password != "" && s.cfg.Admin.PasswordHash == "" {
		if hash, err := HashAdminPassword(s.cfg.Admin.Password); err == nil {
			s.cfg.Admin.PasswordHash = hash
			s.cfg.Admin.Password = ""
		}
		// Si HashAdminPassword échoue (très improbable), on garde
		// Password en clair : l'auth continue de fonctionner via le
		// fallback CheckPassword.
	}
	return nil
}

// Save writes the current configuration to disk.
//
// Note sécurité : le fichier contient un hash bcrypt + tokens MQTT en
// clair. 0600 serait préférable mais le binaire tourne en root sur la
// cam donc 0644 ne change pas grand-chose vs autres users (il n'y en a
// qu'un). À durcir si on industrialise.
func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(s.path, data, 0600); err != nil {
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
