package publisher

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"

	"github.com/caligone/openqiara/internal/camera"
)

// HomeKitConfig holds HomeKit bridge configuration.
type HomeKitConfig struct {
	Pin     string       `json:"pin"`              // Setup code, e.g. "00102003"
	Name    string       `json:"name"`             // Bridge name, e.g. "OpenQiara"
	DataDir string       `json:"data_dir"`         // Persistent storage path
	Port    string       `json:"port"`             // Listen port, e.g. "" for default
	Camera  CameraConfig `json:"camera,omitempty"` // optional IP Camera accessory

	// ExposeAlarm controls whether the SecuritySystem (alarm panel) accessory
	// is published. In alarmo mode, Alarmo already exposes its own panel via
	// HA's HomeKit integration — duplicating it here would show 2 alarms in
	// the Home app. Set to false when config.Alarm.Mode == "alarmo".
	ExposeAlarm bool `json:"expose_alarm,omitempty"`
}

// HomeKit accessory ID layout. AIDs MUST be stable across reboots and
// across rebuilds — iOS identifies a paired accessory by its AID, so any
// change makes it appear "Pas de réponse" until re-pairing.
//
//	1               = bridge (HAP convention)
//	aidAlarm        = security system (KPD/alarm) — fixed, max 1 per bridge
//	aidCamera       = camera accessory — fixed, max 1 per bridge
//	aidSensorBase+ID = DWS / PIR / SRN sensors — derived from sensor.ID
//
// The +1000 offset on sensors leaves room for fixed-AID accessories and
// makes collisions impossible as long as Qiara sensor IDs stay below 1000
// (typical IDs observed: 12, 17, 23 — well within range).
const (
	aidAlarm      uint64 = 10
	aidCamera     uint64 = 11
	aidShutter    uint64 = 12
	aidSensorBase uint64 = 1000
)

// HomeKitPublisher exposes sensors as HomeKit accessories via brutella/hap.
//
// The bridge can be rebuilt at runtime via RebuildBridge when sensors are
// paired or removed. Rebuild reuses the same FsStore so the iOS pairing
// survives, and AIDs stay deterministic so existing accessories aren't
// orphaned.
type HomeKitPublisher struct {
	cfg HomeKitConfig
	log *slog.Logger

	// Sticky context from the original Start call. RebuildBridge derives
	// each server's per-instance context from this so cancelling the
	// outer context still tears everything down.
	rootCtx context.Context

	// Last known sensor list passed to Start/RebuildBridge. Used to
	// short-circuit no-op rebuilds (rename, state change).
	lastSensors []camera.Sensor

	// Command handler shared with all rebuild iterations.
	cmds *CommandHandler

	mu      sync.Mutex
	server  *hap.Server
	cancel  context.CancelFunc // cancels the current server's goroutine

	// Accessories by sensor ID — replaced on every rebuild.
	contacts map[int]*accessory.ContactSensor
	motions  map[int]*accessory.MotionSensor
	switches map[int]*accessory.Switch // sirens
	alarm    *accessory.SecuritySystem
	camera   *HomeKitCamera // optional, nil unless cfg.Camera.Enabled
}

// NewHomeKitPublisher creates a new HomeKit publisher.
func NewHomeKitPublisher(cfg HomeKitConfig, logger *slog.Logger) *HomeKitPublisher {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Pin == "" {
		cfg.Pin = "00102003"
	}
	if cfg.Name == "" {
		cfg.Name = "OpenQiara"
	}
	if cfg.DataDir == "" {
		cfg.DataDir = "/data/homekit"
	}
	return &HomeKitPublisher{
		cfg:      cfg,
		log:      logger,
		contacts: make(map[int]*accessory.ContactSensor),
		motions:  make(map[int]*accessory.MotionSensor),
		switches: make(map[int]*accessory.Switch),
	}
}

func (p *HomeKitPublisher) Start(ctx context.Context, sensors []camera.Sensor, cmds *CommandHandler) error {
	p.rootCtx = ctx
	p.cmds = cmds

	if err := p.buildAndServe(sensors); err != nil {
		return err
	}

	// Note: we used to add a second mDNS responder via hashicorp/mdns
	// here, but it created a conflict with the brutella/dnssd responder
	// that brutella/hap starts internally — both announced the same
	// _hap._tcp service and iOS would get inconsistent info after
	// ~12h of uptime, resulting in the bridge becoming "Not responding".

	return nil
}

func (p *HomeKitPublisher) PublishSensorState(ctx context.Context, sensor camera.Sensor) error {
	switch sensor.Type {
	case "DWS":
		if a, ok := p.contacts[sensor.ID]; ok {
			state := characteristic.ContactSensorStateContactDetected
			if sensor.Open {
				state = characteristic.ContactSensorStateContactNotDetected
			}
			a.ContactSensor.ContactSensorState.SetValue(state)
		}
	case "PIR":
		if a, ok := p.motions[sensor.ID]; ok {
			a.MotionSensor.MotionDetected.SetValue(sensor.Motion)
		}
	}
	return nil
}

func (p *HomeKitPublisher) PublishAlarmState(ctx context.Context, state string) error {
	if p.alarm == nil {
		return nil
	}
	// HomeKit Security System has two characteristics: Current (the
	// actual state right now) and Target (what the user wants). The
	// iOS Home tile reads Target to colour the icon, so we must update
	// both — otherwise the tile stays on the previous target value
	// even though Current was updated.
	var current, target int
	switch state {
	case "armed_away":
		current = characteristic.SecuritySystemCurrentStateAwayArm
		target = characteristic.SecuritySystemTargetStateAwayArm
	case "armed_night":
		current = characteristic.SecuritySystemCurrentStateNightArm
		target = characteristic.SecuritySystemTargetStateNightArm
	case "armed_home", "armed_stay":
		current = characteristic.SecuritySystemCurrentStateStayArm
		target = characteristic.SecuritySystemTargetStateStayArm
	case "disarmed":
		current = characteristic.SecuritySystemCurrentStateDisarmed
		target = characteristic.SecuritySystemTargetStateDisarm
	case "triggered":
		current = characteristic.SecuritySystemCurrentStateAlarmTriggered
		// Target stays at the last armed state when triggered.
		target = -1
	default:
		return nil
	}
	p.alarm.SecuritySystem.SecuritySystemCurrentState.SetValue(current)
	if target >= 0 {
		p.alarm.SecuritySystem.SecuritySystemTargetState.SetValue(target)
	}
	return nil
}

func (p *HomeKitPublisher) Close() error {
	// hap.Server is stopped by context cancellation
	return nil
}

// buildAndServe constructs the accessory list from sensors, opens a new
// hap.Server on the persistent FsStore, and starts it. Safe to call
// multiple times: each call cancels the previous server first. AIDs are
// derived from sensor.ID so iOS keeps recognising paired accessories
// across rebuilds.
func (p *HomeKitPublisher) buildAndServe(sensors []camera.Sensor) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Reset accessory maps — we're rebuilding from scratch.
	p.contacts = make(map[int]*accessory.ContactSensor)
	p.motions = make(map[int]*accessory.MotionSensor)
	p.switches = make(map[int]*accessory.Switch)
	p.alarm = nil

	bridge := accessory.NewBridge(accessory.Info{
		Name:         p.cfg.Name,
		Manufacturer: "OpenQiara",
		Model:        "Qiara Camera",
	})

	var accs []*accessory.A

	for _, s := range sensors {
		switch s.Type {
		case "DWS":
			name := s.Label
			if name == "" {
				name = fmt.Sprintf("Porte %d", s.ID)
			}
			a := accessory.NewContactSensor(accessory.Info{Name: name, Manufacturer: "Qiara"})
			a.A.Id = aidSensorBase + uint64(s.ID)
			p.contacts[s.ID] = a
			accs = append(accs, a.A)

		case "PIR":
			name := s.Label
			if name == "" {
				name = fmt.Sprintf("Mouvement %d", s.ID)
			}
			a := accessory.NewMotionSensor(accessory.Info{Name: name, Manufacturer: "Qiara"})
			a.A.Id = aidSensorBase + uint64(s.ID)
			p.motions[s.ID] = a
			accs = append(accs, a.A)

		case "SRN":
			name := s.Label
			if name == "" {
				name = fmt.Sprintf("Sirène %d", s.ID)
			}
			a := accessory.NewSwitch(accessory.Info{Name: name, Manufacturer: "Qiara"})
			a.A.Id = aidSensorBase + uint64(s.ID)
			if p.cmds != nil && p.cmds.OnSirenCommand != nil {
				sensorID := s.ID
				a.Switch.On.OnValueRemoteUpdate(func(on bool) {
					p.cmds.OnSirenCommand(sensorID, on)
				})
			}
			p.switches[s.ID] = a
			accs = append(accs, a.A)

		case "KPD":
			// KPD physique : pas d'accessory HomeKit dédié — l'alarme HK est un
			// SecuritySystem unique exposé une fois, indépendamment du KPD
			// (cf. bloc après cette boucle).
		}
	}

	// SecuritySystem (panneau d'alarme) — un seul par bridge, indépendant des
	// capteurs. Affiché en mode standalone ; skip en mode alarmo (Alarmo
	// expose déjà son propre panel via HA's HomeKit bridge).
	if p.cfg.ExposeAlarm {
		a := accessory.NewSecuritySystem(accessory.Info{
			Name:         "Alarme OpenQiara",
			Manufacturer: "OpenQiara",
		})
		a.A.Id = aidAlarm
		// Restreint l'app Maison aux 3 modes qu'on supporte (pas de "Au domicile",
		// redondant avec "Absent" dans notre modèle). HK utilise ValidValues pour
		// masquer les autres dans l'UI.
		a.SecuritySystem.SecuritySystemTargetState.ValidVals = []int{
			characteristic.SecuritySystemTargetStateAwayArm,
			characteristic.SecuritySystemTargetStateNightArm,
			characteristic.SecuritySystemTargetStateDisarm,
		}
		a.SecuritySystem.SecuritySystemCurrentState.ValidVals = []int{
			characteristic.SecuritySystemCurrentStateAwayArm,
			characteristic.SecuritySystemCurrentStateNightArm,
			characteristic.SecuritySystemCurrentStateDisarmed,
			characteristic.SecuritySystemCurrentStateAlarmTriggered,
		}
		if p.cmds != nil && p.cmds.OnAlarmCommand != nil {
			a.SecuritySystem.SecuritySystemTargetState.OnValueRemoteUpdate(func(state int) {
				var cmd string
				switch state {
				case characteristic.SecuritySystemTargetStateAwayArm:
					cmd = "armed_away"
				case characteristic.SecuritySystemTargetStateNightArm:
					cmd = "armed_night"
				case characteristic.SecuritySystemTargetStateDisarm:
					cmd = "disarmed"
				}
				if cmd != "" {
					p.cmds.OnAlarmCommand(cmd)
				}
			})
		}
		p.alarm = a
		accs = append(accs, a.A)
		p.log.Info("homekit: security system added")
	}

	// Shutter switch.
	if p.cmds != nil && p.cmds.OnShutterCommand != nil {
		shutter := accessory.NewSwitch(accessory.Info{
			Name:         "Cache objectif",
			Manufacturer: "OpenQiara",
		})
		shutter.A.Id = aidShutter
		shutter.Switch.On.OnValueRemoteUpdate(func(on bool) {
			p.cmds.OnShutterCommand(on)
		})
		accs = append(accs, shutter.A)
		p.log.Info("homekit: shutter switch added")
	}

	// Optional IP Camera accessory at fixed AID (only one camera per bridge).
	if p.cfg.Camera.Enabled {
		p.camera = NewHomeKitCamera(p.cfg.Camera, p.log)
		camAcc := p.camera.Accessory()
		camAcc.A.Id = aidCamera
		accs = append(accs, camAcc.A)
		p.log.Info("homekit: camera accessory added", "name", p.cfg.Camera.Name)
	}

	if len(accs) == 0 {
		return fmt.Errorf("homekit: no accessories to expose")
	}

	// Ensure data directory exists. The FsStore must be reused across
	// rebuilds — it holds the iOS pairing material.
	os.MkdirAll(filepath.Dir(p.cfg.DataDir), 0755)
	os.MkdirAll(p.cfg.DataDir, 0755)

	fs := hap.NewFsStore(p.cfg.DataDir)
	server, err := hap.NewServer(fs, bridge.A, accs...)
	if err != nil {
		return fmt.Errorf("homekit: create server: %w", err)
	}
	server.Pin = p.cfg.Pin
	if p.cfg.Port != "" {
		server.Addr = ":" + p.cfg.Port
	} else {
		server.Addr = ":51827"
	}

	// Restrict mDNS to the WiFi interface (ssv0) to avoid
	// brutella/dnssd NETLINK infinite loop on embedded systems.
	server.Ifaces = []string{"ssv0"}

	// Cancel the previous server's goroutine if any. The new server
	// derives its context from rootCtx so the outer cancellation still
	// tears it down on shutdown.
	if p.cancel != nil {
		p.cancel()
	}
	srvCtx, cancel := context.WithCancel(p.rootCtx)
	p.cancel = cancel
	p.server = server
	p.lastSensors = sensors

	p.log.Info("homekit bridge starting",
		"name", p.cfg.Name,
		"pin", p.cfg.Pin,
		"accessories", len(accs),
	)

	go func() {
		backoff := 2 * time.Second
		for {
			p.log.Info("homekit: starting HAP server...")
			err := server.ListenAndServe(srvCtx)
			if srvCtx.Err() != nil {
				// Shutdown requested — exit cleanly
				return
			}
			p.log.Error("homekit: HAP server error, restarting", "error", err, "backoff", backoff)
			select {
			case <-srvCtx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}()

	return nil
}

// RebuildBridge restarts the HAP server with a fresh accessory list. Call
// after a sensor is paired or removed. AIDs of unchanged sensors stay
// stable so iOS keeps recognising them; new sensors appear in the Home
// app the next time iOS reconnects to the bridge (a few seconds).
//
// No-op if the sensor list (IDs and types) hasn't changed compared to
// the previous build — avoids tearing down the server on rename / state
// updates.
func (p *HomeKitPublisher) RebuildBridge(sensors []camera.Sensor) error {
	if !p.sensorListChanged(sensors) {
		return nil
	}
	p.log.Info("homekit: rebuilding bridge", "sensors", len(sensors))
	return p.buildAndServe(sensors)
}

// sensorListChanged returns true if the (id, type) set differs from the
// last build. Order, label, and reachability are intentionally ignored.
func (p *HomeKitPublisher) sensorListChanged(next []camera.Sensor) bool {
	p.mu.Lock()
	prev := p.lastSensors
	p.mu.Unlock()

	if len(prev) != len(next) {
		return true
	}
	prevSet := make(map[int]string, len(prev))
	for _, s := range prev {
		prevSet[s.ID] = s.Type
	}
	for _, s := range next {
		if prevSet[s.ID] != s.Type {
			return true
		}
	}
	return false
}
