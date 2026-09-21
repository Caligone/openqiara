package mqtt

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// recordingClient is a mqtt.Client that records what would go on the wire.
// Only Publish needs real behaviour; the rest satisfies the interface.
type recordingClient struct {
	published []recordedPublish
}

type recordedPublish struct {
	topic   string
	retain  bool
	payload string
}

func (c *recordingClient) Publish(topic string, _ byte, retain bool, payload any) mqtt.Token {
	b, _ := payload.([]byte)
	c.published = append(c.published, recordedPublish{topic: topic, retain: retain, payload: string(b)})
	return doneToken{}
}

func (c *recordingClient) IsConnected() bool      { return true }
func (c *recordingClient) IsConnectionOpen() bool { return true }
func (c *recordingClient) Connect() mqtt.Token    { return doneToken{} }
func (c *recordingClient) Disconnect(uint)        {}
func (c *recordingClient) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token {
	return doneToken{}
}
func (c *recordingClient) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	return doneToken{}
}
func (c *recordingClient) Unsubscribe(...string) mqtt.Token        { return doneToken{} }
func (c *recordingClient) AddRoute(string, mqtt.MessageHandler)    {}
func (c *recordingClient) OptionsReader() mqtt.ClientOptionsReader { return mqtt.ClientOptionsReader{} }

// doneToken is an already-completed token with no error.
type doneToken struct{}

func (doneToken) Wait() bool                     { return true }
func (doneToken) WaitTimeout(time.Duration) bool { return true }
func (doneToken) Error() error                   { return nil }
func (doneToken) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func newRecordingPublisher() (*HAPublisher, *recordingClient) {
	c := &recordingClient{}
	return &HAPublisher{
		client: c,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, c
}

// A retained command is replayed by the broker to every subscriber that
// connects, so a retained ARM_AWAY makes Alarmo re-arm from a stale value
// after each Home Assistant restart — observed re-arming, and once firing
// the siren.
func TestPublishCommandIsNeverRetained(t *testing.T) {
	p, c := newRecordingPublisher()

	if err := p.PublishCommand(context.Background(), "alarmo/command", []byte("ARM_AWAY")); err != nil {
		t.Fatalf("PublishCommand: %v", err)
	}

	if len(c.published) != 1 {
		t.Fatalf("got %d publishes, want 1", len(c.published))
	}
	got := c.published[0]
	if got.retain {
		t.Error("command published with retain — the broker would replay it on every reconnect")
	}
	if got.topic != "alarmo/command" || got.payload != "ARM_AWAY" {
		t.Errorf("got topic=%q payload=%q", got.topic, got.payload)
	}
}

// Discovery and state must stay retained: a subscriber that reconnects has
// no other way to learn that the entities exist and where things stand.
func TestPublishRawStaysRetained(t *testing.T) {
	p, c := newRecordingPublisher()

	if err := p.PublishRaw(context.Background(), "homeassistant/x/config", []byte(`{"name":"x"}`)); err != nil {
		t.Fatalf("PublishRaw: %v", err)
	}

	if len(c.published) != 1 {
		t.Fatalf("got %d publishes, want 1", len(c.published))
	}
	if !c.published[0].retain {
		t.Error("discovery published without retain — HA would lose the entity on reconnect")
	}
}

// PublishAlarmState carries a state (disarmed, armed_away…), not a command,
// despite its parameter being named action. It must stay retained.
func TestPublishAlarmStateStaysRetained(t *testing.T) {
	p, c := newRecordingPublisher()
	p.prefix = "openqiara"

	if err := p.PublishAlarmState(context.Background(), 0, "armed_away"); err != nil {
		t.Fatalf("PublishAlarmState: %v", err)
	}

	if len(c.published) != 1 {
		t.Fatalf("got %d publishes, want 1", len(c.published))
	}
	got := c.published[0]
	if !got.retain {
		t.Error("alarm state published without retain")
	}
	if got.topic != "openqiara/alarm/state" {
		t.Errorf("topic = %q", got.topic)
	}
}
