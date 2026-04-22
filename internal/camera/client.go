// Package camera provides communication with the Qiara camera's MCU.
//
// Phase 1: HTTP client to fbxhome API (localhost:10000)
// Phase 2: Direct charmux UDP communication
package camera

import (
	"context"
	"time"
)

// Client communicates with the camera's MCU to manage sensors.
type Client interface {
	// Connect establishes the connection and authenticates.
	Connect(ctx context.Context) error

	// Sensors returns the list of currently paired sensors.
	Sensors(ctx context.Context) ([]Sensor, error)

	// CachedSensors returns the in-memory sensor state without MCU I/O.
	CachedSensors() []Sensor

	// ReadSensor reads the current state of a sensor by node ID.
	ReadSensor(ctx context.Context, nodeID int, endpoints []string) (*Sensor, error)

	// StartPairing initiates sensor pairing for the given type (DWS, PIR, SRN, KPD).
	// fingerprint is the QR code hex (16 chars), required for fbxhome pairing.
	// Returns a session ID for polling.
	StartPairing(ctx context.Context, sensorType string, fingerprint string) (int, error)

	// PollPairing checks the status of an ongoing pairing session.
	// Returns the paired sensor and true when pairing completes.
	PollPairing(ctx context.Context, session int) (*Sensor, bool, error)

	// StopPairing cancels an ongoing pairing session.
	StopPairing(ctx context.Context, session int) error

	// DeleteSensor removes a paired sensor by node ID.
	DeleteSensor(ctx context.Context, nodeID int) error

	// EndpointsRead reads raw endpoints for a node. Used for KPD pwd listing.
	EndpointsRead(ctx context.Context, nodeID int, endpoints []string) ([]EndpointValue, error)

	// EndpointsWrite writes raw endpoints for a node. Used for KPD pwd management.
	EndpointsWrite(ctx context.Context, nodeID int, eps []EndpointWriteEntry) error

	// OpenStream opens the video stream and returns the SRT passphrase.
	OpenStream(ctx context.Context) (StreamInfo, error)

	// SendPKT sends a raw packet on the PKT channel (charmux mode only).
	SendPKT(ctx context.Context, data []byte) error

	// TriggerSiren sends a test command to a siren sensor (discrete sound).
	TriggerSiren(ctx context.Context, sensorID int) error

	// TriggerSirenAlarm fires the full-power wail (intrusion alarm).
	// `duration` est la durée du wail (typiquement la config WailDuration).
	TriggerSirenAlarm(ctx context.Context, sensorID int, duration time.Duration) error

	// StopSiren stops an ongoing siren (test or alarm).
	StopSiren(ctx context.Context, sensorID int) error

	// SetShutter opens or closes the camera shutter.
	SetShutter(ctx context.Context, open bool) error

	// Events returns a channel of sensor state changes.
	// The channel is closed when Close is called.
	Events() <-chan SensorEvent

	// Close shuts down the client and stops polling.
	Close() error
}
