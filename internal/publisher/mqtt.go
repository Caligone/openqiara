package publisher

import (
	"context"
	"log/slog"
	"strings"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/mqtt"
)

// MQTTPublisher wraps the existing HAPublisher to implement the Publisher interface.
type MQTTPublisher struct {
	pub *mqtt.HAPublisher
	cfg mqtt.Config
	log *slog.Logger

	// PublishAlarmEntity controls whether HA auto-discovery publishes an
	// alarm_control_panel entity for openqiara. Set to false when running
	// in "alarmo" mode so Alarmo is the only alarm entity in HA.
	PublishAlarmEntity bool

	kpdID int // first KPD sensor ID (for alarm)
}

// NewMQTTPublisher creates a new MQTT publisher adapter.
// publishAlarmEntity controls whether the alarm_control_panel discovery
// is published (standalone mode) or skipped (alarmo mode).
func NewMQTTPublisher(cfg mqtt.Config, logger *slog.Logger) *MQTTPublisher {
	return &MQTTPublisher{
		pub:                mqtt.NewHAPublisher(logger),
		cfg:                cfg,
		log:                logger,
		PublishAlarmEntity: true,
	}
}

// HAPublisher returns the underlying HAPublisher for backward compatibility.
func (m *MQTTPublisher) HAPublisher() *mqtt.HAPublisher {
	return m.pub
}

func (m *MQTTPublisher) Start(ctx context.Context, sensors []camera.Sensor, cmds *CommandHandler) error {
	if err := m.pub.Connect(ctx, m.cfg, sensors); err != nil {
		return err
	}

	// Find KPD for alarm
	for _, s := range sensors {
		if s.Type == "KPD" {
			m.kpdID = s.ID

			// Publish alarm discovery only in standalone mode. In alarmo mode
			// we don't want a duplicate alarm_control_panel entity in HA.
			if m.PublishAlarmEntity {
				if err := m.pub.PublishAlarmDiscovery(ctx, s); err != nil {
					m.log.Warn("alarm discovery failed", "error", err)
				} else {
					m.pub.PublishAlarmState(ctx, s.ID, "disarmed")
				}
			} else {
				// Remove any stale discovery from a previous standalone run.
				if err := m.pub.RemoveAlarmDiscovery(ctx, s.ID); err != nil {
					m.log.Debug("alarm discovery removal failed", "error", err)
				}
			}

			// Subscribe to alarm commands from HA
			if cmds != nil && cmds.OnAlarmCommand != nil {
				m.pub.SetupAlarmCommandHandler(s.ID, func(command string) {
					var state string
					switch command {
					case "ARM_AWAY":
						state = "armed_away"
					case "ARM_NIGHT":
						state = "armed_night"
					case "DISARM":
						state = "disarmed"
					default:
						return
					}
					cmds.OnAlarmCommand(state)
				}, m.log)
			}
			break
		}
	}

	// Subscribe to siren commands
	if cmds != nil && cmds.OnSirenCommand != nil {
		m.pub.SetupSirenCommandHandler(sensors, func(sensorID int, command string) {
			on := strings.Contains(command, "true") || strings.Contains(command, "ON")
			cmds.OnSirenCommand(sensorID, on)
		}, m.log)
	}

	return nil
}

func (m *MQTTPublisher) PublishSensorState(ctx context.Context, sensor camera.Sensor) error {
	return m.pub.PublishState(ctx, sensor)
}

func (m *MQTTPublisher) PublishAlarmState(ctx context.Context, state string) error {
	if m.kpdID == 0 {
		return nil
	}
	return m.pub.PublishAlarmState(ctx, m.kpdID, state)
}

func (m *MQTTPublisher) Close() error {
	return m.pub.Close()
}
