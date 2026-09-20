package mqtt

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// discardLogger is a no-op logger for tests (publish() logs on success).
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordingClient is a minimal mqtt.Client stub that records Publish calls so
// tests can assert the retain flag. Only the methods HAPublisher.publish uses
// need real behaviour; the rest just satisfy the interface.
type recordingClient struct {
	publishes []recordedPublish
}

type recordedPublish struct {
	topic   string
	retain  bool
	payload []byte
}

func (c *recordingClient) Publish(topic string, _ byte, retain bool, payload any) mqtt.Token {
	b, _ := payload.([]byte)
	c.publishes = append(c.publishes, recordedPublish{topic: topic, retain: retain, payload: b})
	return completedToken{}
}

func (c *recordingClient) IsConnected() bool      { return true }
func (c *recordingClient) IsConnectionOpen() bool { return true }
func (c *recordingClient) Connect() mqtt.Token    { return completedToken{} }
func (c *recordingClient) Disconnect(uint)        {}
func (c *recordingClient) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token {
	return completedToken{}
}
func (c *recordingClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	return completedToken{}
}
func (c *recordingClient) Unsubscribe(...string) mqtt.Token        { return completedToken{} }
func (c *recordingClient) AddRoute(string, mqtt.MessageHandler)    {}
func (c *recordingClient) OptionsReader() mqtt.ClientOptionsReader { return mqtt.ClientOptionsReader{} }

// completedToken is an already-finished mqtt.Token with no error.
type completedToken struct{}

func (completedToken) Wait() bool                     { return true }
func (completedToken) WaitTimeout(time.Duration) bool { return true }
func (completedToken) Error() error                   { return nil }
func (completedToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// TestPublishCommand_IsNotRetained guards the fix for the alarmo re-arm bug:
// a retained command is replayed by the broker on every reconnect, so Alarmo
// would re-arm from a stale ARM_AWAY after each HA restart.
func TestPublishCommand_IsNotRetained(t *testing.T) {
	c := &recordingClient{}
	p := &HAPublisher{client: c, log: discardLogger()}

	if err := p.PublishCommand(context.Background(), "alarmo/command", []byte("ARM_AWAY")); err != nil {
		t.Fatalf("PublishCommand: %v", err)
	}
	if len(c.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(c.publishes))
	}
	got := c.publishes[0]
	if got.retain {
		t.Error("command published retained; commands must never be retained")
	}
	if got.topic != "alarmo/command" || string(got.payload) != "ARM_AWAY" {
		t.Errorf("got topic=%q payload=%q", got.topic, got.payload)
	}
}

// TestPublishRaw_StaysRetained ensures the fix did not weaken discovery/state,
// which must stay retained to survive a broker/HA reconnect.
func TestPublishRaw_StaysRetained(t *testing.T) {
	c := &recordingClient{}
	p := &HAPublisher{client: c, log: discardLogger()}

	if err := p.PublishRaw(context.Background(), "homeassistant/binary_sensor/openqiara_1/config", []byte("{}")); err != nil {
		t.Fatalf("PublishRaw: %v", err)
	}
	if len(c.publishes) != 1 {
		t.Fatalf("got %d publishes, want 1", len(c.publishes))
	}
	if !c.publishes[0].retain {
		t.Error("discovery (PublishRaw) must stay retained")
	}
}
