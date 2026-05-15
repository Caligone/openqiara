package publisher

import (
	"context"
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

// mockPublisher implements Publisher for testing.
type mockPublisher struct {
	started      bool
	sensorStates []camera.Sensor
	alarmStates  []string
	closed       bool
}

func (m *mockPublisher) Start(_ context.Context, _ []camera.Sensor, _ *CommandHandler) error {
	m.started = true
	return nil
}

func (m *mockPublisher) PublishSensorState(_ context.Context, sensor camera.Sensor) error {
	m.sensorStates = append(m.sensorStates, sensor)
	return nil
}

func (m *mockPublisher) PublishAlarmState(_ context.Context, state string) error {
	m.alarmStates = append(m.alarmStates, state)
	return nil
}

func (m *mockPublisher) Close() error {
	m.closed = true
	return nil
}

func TestPublisherInterface(t *testing.T) {
	// Verify mockPublisher implements Publisher
	var _ Publisher = (*mockPublisher)(nil)
}

func TestCommandHandler(t *testing.T) {
	var alarmCmd string
	var sirenID int
	var sirenOn bool

	cmds := &CommandHandler{
		OnAlarmCommand: func(state string) {
			alarmCmd = state
		},
		OnSirenCommand: func(id int, on bool) {
			sirenID = id
			sirenOn = on
		},
	}

	cmds.OnAlarmCommand("armed_away")
	if alarmCmd != "armed_away" {
		t.Errorf("alarm command: got %q, want armed_away", alarmCmd)
	}

	cmds.OnSirenCommand(146, true)
	if sirenID != 146 || !sirenOn {
		t.Errorf("siren command: got id=%d on=%v, want 146/true", sirenID, sirenOn)
	}
}

func TestMultiPublisherDispatch(t *testing.T) {
	pub1 := &mockPublisher{}
	pub2 := &mockPublisher{}
	pubs := []Publisher{pub1, pub2}

	ctx := context.Background()

	// Dispatch sensor state to all publishers
	sensor := camera.Sensor{ID: 86, Type: "DWS", Open: true}
	for _, p := range pubs {
		_ = p.PublishSensorState(ctx, sensor)
	}

	if len(pub1.sensorStates) != 1 || pub1.sensorStates[0].ID != 86 {
		t.Error("pub1 didn't receive sensor state")
	}
	if len(pub2.sensorStates) != 1 || pub2.sensorStates[0].ID != 86 {
		t.Error("pub2 didn't receive sensor state")
	}

	// Dispatch alarm state
	for _, p := range pubs {
		_ = p.PublishAlarmState(ctx, "triggered")
	}

	if len(pub1.alarmStates) != 1 || pub1.alarmStates[0] != "triggered" {
		t.Error("pub1 didn't receive alarm state")
	}
	if len(pub2.alarmStates) != 1 || pub2.alarmStates[0] != "triggered" {
		t.Error("pub2 didn't receive alarm state")
	}
}

func TestHomeKitConfigDefaults(t *testing.T) {
	pub := NewHomeKitPublisher(HomeKitConfig{}, nil)

	if pub.cfg.Pin != "00102003" {
		t.Errorf("default pin: got %q, want 00102003", pub.cfg.Pin)
	}
	if pub.cfg.Name != "OpenQiara" {
		t.Errorf("default name: got %q, want OpenQiara", pub.cfg.Name)
	}
	if pub.cfg.DataDir != "/data/homekit" {
		t.Errorf("default data_dir: got %q, want /data/homekit", pub.cfg.DataDir)
	}
}

func TestHomeKitNoAccessories(t *testing.T) {
	pub := NewHomeKitPublisher(HomeKitConfig{}, nil)
	err := pub.Start(context.Background(), nil, nil)
	if err == nil {
		t.Error("expected error with no sensors")
	}
}
