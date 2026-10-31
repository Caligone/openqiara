// Package config manages persistent configuration stored on the camera.
package config

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
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
	Update     UpdateConfig  `json:"update"`
	Sensors    []SensorEntry `json:"sensors,omitempty"`
	DeletedIDs []int         `json:"deleted_ids,omitempty"`

	// RadioNextAddr is the radio address the next pairing gives (charmux
	// mode). It only grows, like fbxhome's domus_next_addr: the MCU has no
	// way to forget a sensor, so an address is never given twice.
	RadioNextAddr uint32 `json:"radio_next_addr,omitempty"`

	// ShutterClosed is the privacy shutter as last set: closed, hlcamd's
	// streams stay paused, at start too.
	ShutterClosed bool `json:"shutter_closed,omitempty"`
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

// UpdateConfig holds the OTA settings.
type UpdateConfig struct {
	// Auto installs the latest release on its own, only while the alarm is
	// disarmed. Off by default.
	Auto bool `json:"auto"`
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

	// SirenSounds controls the siren, which keeps the alarm's delays.
	// "all"  (default) = its beeps in the exit and entry delays, then the alert
	// "alarm_only"     = quiet delays (not tried on the hardware yet), then the alert
	// "none"           = never armed nor set off
	SirenSounds string `json:"siren_sounds"`

	// ArmingDelaySeconds is the exit delay after arming from the keypad,
	// counted by the siren: 255 s at most (default 60s). 0 means "use
	// default".
	ArmingDelaySeconds int `json:"arming_delay_seconds,omitempty"`

	// PendingDelaySeconds is the entry delay before the alert, counted by
	// the siren: 255 s at most (default 60s). 0 means "use default".
	PendingDelaySeconds int `json:"pending_delay_seconds,omitempty"`

	// WailDurationSeconds is how long the alert lasts, counted by the siren
	// in 2 s steps, 510 s at most (default 300 s). 0 means "use default".
	WailDurationSeconds int `json:"wail_duration_seconds,omitempty"`
}

// Defaults for alarm timings, exposed so other packages don't redeclare them.
const (
	DefaultArmingDelaySeconds  = 60
	DefaultPendingDelaySeconds = 60
	DefaultWailDurationSeconds = 300 // fbxhome's
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
// the camera's H.264 stream and its microphone (AAC-LC) for consumers like
// Scrypted, Frigate or VLC. Disabled by default.
type RTSPConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Listen is the bind address, default ":8554".
	Listen string `json:"listen,omitempty"`
	// Path is the stream path, default "openqiara".
	Path string `json:"path,omitempty"`
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
}

// MQTTConfig holds MQTT broker connection settings.
type MQTTConfig struct {
	Broker      string `json:"broker"`
	Username    string `json:"username,omitempty"`
	Password    string `json:"password,omitempty"`
	TopicPrefix string `json:"topic_prefix,omitempty"`

	// TLS options, used when Broker has an ssl:// (or tls://, mqtts://) scheme.
	// TLSCACert is a PEM CA bundle path to trust the broker certificate (needed
	// for a self-signed or private-CA broker such as a local Mosquitto).
	// TLSClientCert/TLSClientKey enable mutual TLS (client certificate auth).
	// TLSInsecure disables certificate verification — test only, never for a
	// real deployment.
	TLSCACert     string `json:"tls_ca_cert,omitempty"`
	TLSClientCert string `json:"tls_client_cert,omitempty"`
	TLSClientKey  string `json:"tls_client_key,omitempty"`
	TLSInsecure   bool   `json:"tls_insecure,omitempty"`
}

// HasTLS reports whether any file-configured TLS option is set.
func (m MQTTConfig) HasTLS() bool {
	return m.TLSCACert != "" || m.TLSClientCert != "" || m.TLSClientKey != "" || m.TLSInsecure
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
type SensorEntry struct {
	ID           int    `json:"id"`
	Type         string `json:"type"`
	Model        string `json:"model"`
	Label        string `json:"label,omitempty"`
	NightAllowed bool   `json:"night_allowed,omitempty"` // ignored by the alarm when armed for the night
	Instant      bool   `json:"instant,omitempty"`       // fires the siren with no entry delay

	// KPDCodes are the codes a keypad accepts: it gets the list each time
	// it wakes up and checks a typed code against it itself.
	KPDCodes []KPDCode `json:"kpd_codes,omitempty"`

	// Radio is where the sensor sits on the radio network: set at
	// pairing, imported from fbxhome.xml at the first start.
	Radio RadioNode `json:"radio,omitzero"`
	// Battery is the last raw level the sensor reported (0..255), kept
	// across restarts: sensors report it every few hours only.
	Battery int `json:"battery,omitempty"`
	// Temperature is the last one the sensor reported, in °C; nil before
	// the first. Kept across restarts, sensors send it rarely.
	Temperature *int `json:"temperature,omitempty"`
}

// KPDCode is a code a keypad accepts.
type KPDCode struct {
	Password string `json:"password"` // 4 digits
	Label    string `json:"label,omitempty"`
}

// RadioNode is a sensor's place on the radio network. Addr 0 means none.
type RadioNode struct {
	Addr        uint32 `json:"addr"`         // assigned at pairing
	UID         string `json:"uid"`          // hardware id from the pairing beacon, hex
	SystemIndex uint8  `json:"system_index"` // 0..63, sent in the sensor's config
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
	migrateNightAlarm(&s.cfg, data)
	migrateKPDCode(&s.cfg, data)

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

// migrateNightAlarm folds night_alarm, the fbxhome flag "triggers when
// armed for the night", into night_allowed, its opposite, which it
// overrode. The other fbxhome flags (day_alarm, day_timed, night_timed)
// never reached the alarm: they are dropped at the next save.
func migrateNightAlarm(cfg *Config, data []byte) {
	var legacy struct {
		Sensors []struct {
			ID         int   `json:"id"`
			NightAlarm *bool `json:"night_alarm"`
		} `json:"sensors"`
	}
	if json.Unmarshal(data, &legacy) != nil {
		return
	}
	for _, l := range legacy.Sensors {
		if l.NightAlarm == nil {
			continue
		}
		for i := range cfg.Sensors {
			if cfg.Sensors[i].ID == l.ID {
				cfg.Sensors[i].NightAllowed = !*l.NightAlarm
			}
		}
	}
}

// migrateKPDCode turns kpd_code, a keypad's single code before
// kpd_codes, into its code list; kpd_codes wins when both are there.
// kpd_code is dropped at the next save.
func migrateKPDCode(cfg *Config, data []byte) {
	var legacy struct {
		Sensors []struct {
			ID    int    `json:"id"`
			Code  string `json:"kpd_code"`
			Label string `json:"kpd_code_label"`
		} `json:"sensors"`
	}
	if json.Unmarshal(data, &legacy) != nil {
		return
	}
	for _, l := range legacy.Sensors {
		if l.Code == "" {
			continue
		}
		for i := range cfg.Sensors {
			if se := &cfg.Sensors[i]; se.ID == l.ID && len(se.KPDCodes) == 0 {
				se.KPDCodes = []KPDCode{{Password: l.Code, Label: l.Label}}
			}
		}
	}
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
	return s.save()
}

// save writes the file whole or not at all: a full /data or a power cut
// leaves the previous config, never a truncated one. In charmux mode it
// is the registry of the paired sensors.
func (s *Store) save() error {
	data, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err == nil {
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		err = errors.Join(err, f.Close())
	}
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// Get returns a copy of the current configuration. The copy shares
// nothing with the store: Update may change sensors in place.
func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.clone()
}

// Update applies a mutation function to the configuration and saves it.
// If the save fails, the mutation is undone.
func (s *Store) Update(fn func(*Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cfg.clone()
	fn(&s.cfg)
	if err := s.save(); err != nil {
		s.cfg = old
		return err
	}
	return nil
}

func (c Config) clone() Config {
	c.Sensors = slices.Clone(c.Sensors)
	for i := range c.Sensors {
		c.Sensors[i].KPDCodes = slices.Clone(c.Sensors[i].KPDCodes)
	}
	c.DeletedIDs = slices.Clone(c.DeletedIDs)
	return c
}
