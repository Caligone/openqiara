package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/caligone/openqiara/internal/camera"
)

const (
	qos           byte = 1
	retained           = true
	connectTimeout     = 10 * time.Second
	publishTimeout     = 5 * time.Second
)

// HAPublisher implements Publisher for Home Assistant MQTT auto-discovery.
type HAPublisher struct {
	mu     sync.Mutex
	client mqtt.Client
	prefix string // topic prefix, default "openqiara"
	log    *slog.Logger
}

// NewHAPublisher creates a new HAPublisher.
// If logger is nil, slog.Default() is used.
func NewHAPublisher(logger *slog.Logger) *HAPublisher {
	if logger == nil {
		logger = slog.Default()
	}
	return &HAPublisher{log: logger}
}

// Connect establishes the MQTT connection and publishes initial discovery configs.
func (p *HAPublisher) Connect(ctx context.Context, cfg Config, sensors []camera.Sensor) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	prefix := cfg.TopicPrefix
	if prefix == "" {
		prefix = "openqiara"
	}
	p.prefix = prefix

	opts := mqtt.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID("openqiara").
		SetCleanSession(false).
		SetAutoReconnect(true).
		SetConnectTimeout(connectTimeout).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			p.log.Warn("mqtt connection lost", "error", err)
		}).
		SetOnConnectHandler(func(_ mqtt.Client) {
			p.log.Info("mqtt connected", "broker", cfg.Broker)
		})

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}

	client := mqtt.NewClient(opts)
	token := client.Connect()
	if !token.WaitTimeout(connectTimeout) {
		return fmt.Errorf("mqtt connect timeout")
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt connect: %w", err)
	}

	p.client = client

	// Publish discovery for all known sensors.
	for _, s := range sensors {
		if err := p.publishDiscovery(ctx, s); err != nil {
			p.log.Warn("discovery publish failed", "sensor_id", s.ID, "error", err)
		}
		// Publish extra entities (battery, temperature)
		for _, extra := range buildExtraDiscoveryTopics(p.prefix, s) {
			payload, err := json.Marshal(extra.Payload)
			if err != nil {
				continue
			}
			if err := p.publish(ctx, extra.Topic, payload, retained); err != nil {
				p.log.Warn("extra discovery publish failed", "topic", extra.Topic, "error", err)
			}
		}
	}

	// Publish shutter discovery (camera stream is added as generic camera in HA)
	shutterTopic, shutterPayload := ShutterDiscoveryPayload(p.prefix)
	if err := p.publish(ctx, shutterTopic, shutterPayload, retained); err != nil {
		p.log.Warn("shutter discovery failed", "error", err)
	}

	return nil
}

// PublishShutterState publishes the shutter state to MQTT.
func (p *HAPublisher) PublishShutterState(ctx context.Context, open bool) error {
	state := "closed"
	if open {
		state = "open"
	}
	payload, _ := json.Marshal(map[string]string{"state": state})
	return p.publish(ctx, p.prefix+"/shutter/state", payload, true)
}

// Subscribe subscribes to a raw MQTT topic with the given handler.
// Used for custom integrations like listening to alarmo/state.
func (p *HAPublisher) Subscribe(topic string, handler func(topic string, payload []byte)) {
	if p.client == nil || !p.client.IsConnected() {
		return
	}
	p.client.Subscribe(topic, 0, func(_ mqtt.Client, msg mqtt.Message) {
		handler(msg.Topic(), msg.Payload())
	})
}

// Unsubscribe removes a previous subscription.
func (p *HAPublisher) Unsubscribe(topic string) {
	if p.client == nil || !p.client.IsConnected() {
		return
	}
	p.client.Unsubscribe(topic)
}

// SetupShutterCommandHandler subscribes to shutter commands from HA.
func (p *HAPublisher) SetupShutterCommandHandler(callback func(open bool), logger *slog.Logger) {
	topic := p.prefix + "/shutter/set"
	p.client.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		cmd := string(msg.Payload())
		logger.Info("shutter command received", "command", cmd)
		switch cmd {
		case "open", "OPEN":
			callback(true)
		case "close", "closed", "CLOSE":
			callback(false)
		}
	})
}

// PublishState sends a sensor state update to MQTT.
func (p *HAPublisher) PublishState(ctx context.Context, sensor camera.Sensor) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	payload, err := marshalState(sensor)
	if err != nil {
		return err
	}

	topic := stateTopic(p.prefix, sensor.ID)
	return p.publish(ctx, topic, payload, retained)
}

// PublishDiscovery sends HA auto-discovery config for a sensor.
func (p *HAPublisher) PublishDiscovery(ctx context.Context, sensor camera.Sensor) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishDiscovery(ctx, sensor)
}

// publishDiscovery is the internal version (caller must hold mu).
func (p *HAPublisher) publishDiscovery(ctx context.Context, sensor camera.Sensor) error {
	topic, ok := buildDiscoveryTopic(sensor)
	if !ok {
		p.log.Debug("skipping discovery for unsupported sensor type", "type", sensor.Type, "id", sensor.ID)
		return nil
	}

	disc, _ := buildDiscoveryPayload(p.prefix, sensor)
	payload, err := json.Marshal(disc)
	if err != nil {
		return fmt.Errorf("marshal discovery: %w", err)
	}

	return p.publish(ctx, topic, payload, retained)
}

// RemoveDiscovery removes a sensor from HA by publishing empty retained messages on all discovery topics.
func (p *HAPublisher) RemoveDiscovery(ctx context.Context, sensorID int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Remove main entity + battery + temperature for all possible domains.
	for _, meta := range sensorTypeMap {
		topic := fmt.Sprintf("homeassistant/%s/openqiara_%d/config", meta.domain, sensorID)
		_ = p.publish(ctx, topic, []byte{}, retained)
	}
	_ = p.publish(ctx, fmt.Sprintf("homeassistant/sensor/openqiara_%d_battery/config", sensorID), []byte{}, retained)
	_ = p.publish(ctx, fmt.Sprintf("homeassistant/sensor/openqiara_%d_temperature/config", sensorID), []byte{}, retained)
	// Also clear the state topic
	_ = p.publish(ctx, fmt.Sprintf("%s/sensor/%d/state", p.prefix, sensorID), []byte{}, retained)
	return nil
}

// SetupSirenCommandHandler subscribes to siren command topics.
// callback receives (sensorID, "on" or "off").
func (p *HAPublisher) SetupSirenCommandHandler(sensors []camera.Sensor, callback func(sensorID int, command string), logger *slog.Logger) {
	for _, s := range sensors {
		if s.Type != "SRN" {
			continue
		}
		topic := fmt.Sprintf("%s/siren/%d/set", p.prefix, s.ID)
		sensorID := s.ID
		p.client.Subscribe(topic, qos, func(_ mqtt.Client, msg mqtt.Message) {
			cmd := string(msg.Payload())
			logger.Info("siren command from HA", "command", cmd, "sensor_id", sensorID)
			if callback != nil {
				callback(sensorID, cmd)
			}
		})
		logger.Info("subscribed to siren commands", "topic", topic, "sensor_id", s.ID)
	}
}

// PublishSirenState publishes the siren active state.
func (p *HAPublisher) PublishSirenState(ctx context.Context, sensorID int, active bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	payload, _ := json.Marshal(srnState{Active: active})
	topic := stateTopic(p.prefix, sensorID)
	return p.publish(ctx, topic, payload, retained)
}

// Prefix returns the configured topic prefix.
func (p *HAPublisher) Prefix() string {
	return p.prefix
}

// PublishRaw publishes a raw payload to a topic with retain.
func (p *HAPublisher) PublishRaw(ctx context.Context, topic string, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publish(ctx, topic, payload, retained)
}

// Close disconnects the MQTT client.
func (p *HAPublisher) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.client != nil && p.client.IsConnected() {
		p.client.Disconnect(250)
	}
	return nil
}

// publish sends a message and respects context cancellation.
func (p *HAPublisher) publish(_ context.Context, topic string, payload []byte, retain bool) error {
	if p.client == nil || !p.client.IsConnected() {
		return fmt.Errorf("mqtt client not connected")
	}

	token := p.client.Publish(topic, qos, retain, payload)
	if !token.WaitTimeout(publishTimeout) {
		return fmt.Errorf("mqtt publish timeout on %s", topic)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt publish %s: %w", topic, err)
	}

	p.log.Info("mqtt published", "topic", topic, "size", len(payload))
	return nil
}

// marshalState returns the JSON state payload for a sensor.
// Temperature is stored as int (value*10) in the camera package; we convert to float64 Celsius.
func marshalState(s camera.Sensor) ([]byte, error) {
	temp := float64(s.Temperature) / 10.0
	var v any
	switch s.Type {
	case "DWS":
		v = dwsState{Open: s.Open, Battery: s.Battery, Temperature: temp, Reachable: s.Reachable}
	case "PIR":
		v = pirState{Motion: s.Motion, Battery: s.Battery, Temperature: temp, Reachable: s.Reachable}
	case "SRN":
		v = srnState{Active: false, Battery: s.Battery, Reachable: s.Reachable}
	case "KPD":
		v = kpdState{Battery: s.Battery, Reachable: s.Reachable}
	default:
		return nil, fmt.Errorf("unknown sensor type: %s", s.Type)
	}
	return json.Marshal(v)
}
