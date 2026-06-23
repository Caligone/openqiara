package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/caligone/openqiara/internal/camera"
)

// alarmDiscoveryPayload is the HA MQTT auto-discovery config for alarm_control_panel.
type alarmDiscoveryPayload struct {
	Name          string   `json:"name"`
	UniqueID      string   `json:"unique_id"`
	StateTopic    string   `json:"state_topic"`
	CommandTopic  string   `json:"command_topic"`
	CodeArmReq    bool     `json:"code_arm_required"`
	CodeDisarmReq bool     `json:"code_disarm_required"`
	Device        haDevice `json:"device"`
}

// PublishAlarmDiscovery publishes HA auto-discovery for a KPD as alarm_control_panel.
func (p *HAPublisher) PublishAlarmDiscovery(ctx context.Context, sensor camera.Sensor) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	name := sensorDisplayName(sensor)
	topic := fmt.Sprintf("homeassistant/alarm_control_panel/openqiara_%d/config", sensor.ID)

	disc := alarmDiscoveryPayload{
		Name:         "OpenQiara Alarm",
		UniqueID:     "openqiara_alarm",
		StateTopic:   fmt.Sprintf("%s/alarm/state", p.prefix),
		CommandTopic: fmt.Sprintf("%s/alarm/set", p.prefix),
		CodeArmReq:   false,
		CodeDisarmReq: false,
		Device: haDevice{
			Identifiers:  []string{"openqiara_alarm"},
			Name:         "OpenQiara Alarm",
			Manufacturer: "OpenQiara",
			Model:        "DomusRF Alarm",
		},
	}
	_ = name

	payload, err := json.Marshal(disc)
	if err != nil {
		return fmt.Errorf("marshal alarm discovery: %w", err)
	}

	return p.publish(ctx, topic, payload, retained)
}

// RemoveAlarmDiscovery publishes an empty retained payload on the alarm
// discovery topic, which instructs HA to forget the entity. Used when
// switching from standalone to alarmo mode.
func (p *HAPublisher) RemoveAlarmDiscovery(ctx context.Context, sensorID int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	topic := fmt.Sprintf("homeassistant/alarm_control_panel/openqiara_%d/config", sensorID)
	return p.publish(ctx, topic, []byte(""), retained)
}

// PublishAlarmState publishes the current alarm state.
// action should be one of: "disarmed", "armed_away", "armed_night", "triggered"
func (p *HAPublisher) PublishAlarmState(ctx context.Context, _ int, action string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	topic := fmt.Sprintf("%s/alarm/state", p.prefix)
	if err := p.publish(ctx, topic, []byte(action), retained); err != nil {
		return err
	}
	// TODO: Alarmo integration — need to determine correct command format
	// (plain string vs JSON with code) for alarmo/command topic.
	return nil
}

// SetupAlarmCommandHandler subscribes to the command topic and calls the callback
// when HA sends a command (e.g., arm_away, disarm).
func (p *HAPublisher) SetupAlarmCommandHandler(sensorID int, callback func(command string), logger *slog.Logger) {
	topic := fmt.Sprintf("%s/alarm/set", p.prefix)
	if p.client == nil {
		logger.Warn("MQTT client nil, cannot subscribe to alarm commands")
		return
	}
	// Don't gate on IsConnected: paho (CleanSession=false) queues the
	// subscription and (re)establishes it on connect, so it survives a broker
	// that was down at boot and later reconnects.

	handler := func(_ paho.Client, msg paho.Message) {
		cmd := string(msg.Payload())
		logger.Info("alarm command received", "command", cmd, "topic", msg.Topic())
		if callback != nil {
			callback(cmd)
		}
	}

	p.client.Subscribe(topic, qos, handler)
	logger.Info("subscribed to alarm commands", "topic", topic)
}
