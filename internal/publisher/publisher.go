// Package publisher defines the interface for state synchronization backends
// (MQTT, HomeKit, etc.). Each publisher translates sensor/alarm events into
// its protocol and handles incoming commands (arm/disarm, siren on/off).
package publisher

import (
	"context"

	"github.com/caligone/openqiara/internal/camera"
)

// CommandHandler receives commands from external systems (HA, HomeKit, etc.).
type CommandHandler struct {
	// OnAlarmCommand is called when an external system sends arm/disarm.
	// state is one of: "armed_away", "armed_night", "disarmed".
	OnAlarmCommand func(state string)

	// OnSirenCommand is called when an external system toggles the siren.
	OnSirenCommand func(sensorID int, on bool)

	// OnShutterCommand is called when an external system opens/closes the shutter.
	OnShutterCommand func(open bool)
}

// Publisher synchronizes sensor state with an external system.
type Publisher interface {
	// Start connects and publishes initial discovery/state for all sensors.
	Start(ctx context.Context, sensors []camera.Sensor, cmds *CommandHandler) error

	// PublishSensorState sends a sensor state update.
	PublishSensorState(ctx context.Context, sensor camera.Sensor) error

	// PublishAlarmState sends the alarm panel state.
	PublishAlarmState(ctx context.Context, state string) error

	// Close shuts down the publisher.
	Close() error
}
