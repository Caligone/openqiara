// Package mqtt publishes sensor state to Home Assistant via MQTT auto-discovery.
package mqtt

import (
	"context"

	"github.com/caligone/openqiara/internal/camera"
)

// Config holds MQTT broker connection settings.
type Config struct {
	Broker      string `json:"broker"` // e.g. "tcp://192.168.1.42:1883" or "ssl://192.168.1.42:8883"
	Username    string `json:"username"`
	Password    string `json:"password"`
	TopicPrefix string `json:"topic_prefix"` // default: "openqiara"

	// TLS options, applied when the broker scheme is ssl:// (tls://, mqtts://).
	TLSCACert     string `json:"tls_ca_cert"`     // PEM CA bundle to trust the broker cert
	TLSClientCert string `json:"tls_client_cert"` // client cert for mutual TLS
	TLSClientKey  string `json:"tls_client_key"`  // client key for mutual TLS
	TLSInsecure   bool   `json:"tls_insecure"`    // skip cert verification (test only)
}

// Publisher sends sensor states and discovery messages to MQTT.
type Publisher interface {
	// Connect establishes the MQTT connection and publishes discovery messages.
	Connect(ctx context.Context, cfg Config, sensors []camera.Sensor) error

	// PublishState sends a sensor state update.
	PublishState(ctx context.Context, sensor camera.Sensor) error

	// PublishDiscovery sends HA auto-discovery config for a sensor.
	PublishDiscovery(ctx context.Context, sensor camera.Sensor) error

	// RemoveDiscovery removes a sensor from HA.
	RemoveDiscovery(ctx context.Context, sensorID int) error

	// Close disconnects.
	Close() error
}
