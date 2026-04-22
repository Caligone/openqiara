package camera

import (
	"context"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
)

// fakeMux is a minimal charmux.Client stub for unit tests.
// We can't use the real client (it binds UDP ports), so we test
// the parsing/logic layers directly and use fakeMux only for
// integration-style tests via the exported interface.

func TestParseNodeTable_ValidTable(t *testing.T) {
	// Simulated 74-byte node table:
	// Header: 07 08
	// Entry 1 (addr=2, type=DWS=0x04): 02 00 00 04 00 00 00 00 00
	// Entry 2 (addr=7, type=KPD=0x06): 07 00 00 06 00 00 00 00 00
	// Remaining: zero-padded to 74 bytes.
	raw := make([]byte, 74)
	raw[0] = 0x07 // opcode
	raw[1] = 0x08 // count/flags

	// Entry at offset 2: addr=2, type=DWS
	raw[2] = 0x02
	raw[5] = nodeTypeDWS

	// Entry at offset 11: addr=7, type=KPD
	raw[11] = 0x07
	raw[14] = nodeTypeKPD

	sensors := parseNodeTable(raw)

	if len(sensors) != 2 {
		t.Fatalf("got %d sensors, want 2", len(sensors))
	}

	dws, ok := sensors[2]
	if !ok {
		t.Fatal("sensor addr=2 not found")
	}
	if dws.Type != "UNKNOWN" {
		t.Errorf("sensor 2 type = %q, want UNKNOWN", dws.Type)
	}
	if dws.ID != 2 {
		t.Errorf("sensor 2 ID = %d, want 2", dws.ID)
	}

	kpd, ok := sensors[7]
	if !ok {
		t.Fatal("sensor addr=7 not found")
	}
	if kpd.Type != "UNKNOWN" {
		t.Errorf("sensor 7 type = %q, want UNKNOWN", kpd.Type)
	}
}

func TestParseNodeTable_EmptyTable(t *testing.T) {
	raw := make([]byte, 74)
	raw[0] = 0x07
	raw[1] = 0x00

	sensors := parseNodeTable(raw)
	if len(sensors) != 0 {
		t.Errorf("got %d sensors, want 0", len(sensors))
	}
}

func TestParseNodeTable_WrongOpcode(t *testing.T) {
	raw := []byte{0x02, 0x08}
	sensors := parseNodeTable(raw)
	if len(sensors) != 0 {
		t.Errorf("got %d sensors from bad opcode, want 0", len(sensors))
	}
}

func TestParseNodeTable_TooShort(t *testing.T) {
	sensors := parseNodeTable([]byte{0x07})
	if len(sensors) != 0 {
		t.Errorf("got %d sensors from short data, want 0", len(sensors))
	}
}

func TestIsDWSEvent(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    bool
	}{
		{"open", []byte{0x55, 0x10, 0x00, 0x01}, true},
		{"close", []byte{0x55, 0x10, 0x40, 0x00}, true},
		{"too short", []byte{0x55, 0x01}, false},
		{"not DWS", []byte{0x55, 0x09}, false},
		{"kpd command", []byte{0x55, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isDWSEvent(tt.payload)
			if got != tt.want {
				t.Errorf("isDWSEvent(%x) = %v, want %v", tt.payload, got, tt.want)
			}
		})
	}
}

func TestIsDWSOpen(t *testing.T) {
	if !isDWSOpen([]byte{0x55, 0x10, 0x40, 0x00}) {
		t.Error("expected open for 40 00")
	}
	if isDWSOpen([]byte{0x55, 0x10, 0x00, 0x01}) {
		t.Error("expected closed for 00 01")
	}
}

func TestIsKPDHeartbeat(t *testing.T) {
	if !isKPDHeartbeat([]byte{0x55, 0x09}) {
		t.Error("expected true for 55 09")
	}
	if isKPDHeartbeat([]byte{0x55, 0x01}) {
		t.Error("expected false for 55 01")
	}
	if isKPDHeartbeat([]byte{0x55}) {
		t.Error("expected false for single byte")
	}
}

func TestIsKPDCommand(t *testing.T) {
	armAway := []byte{0x55, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01, 0x01}
	if !isKPDCommand(armAway) {
		t.Error("expected true for arm away")
	}

	armNight := []byte{0x55, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01, 0x02}
	if !isKPDCommand(armNight) {
		t.Error("expected true for arm night")
	}

	disarm := []byte{0x55, 0x01, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00}
	if !isKPDCommand(disarm) {
		t.Error("expected true for disarm")
	}

	if isKPDCommand([]byte{0x55, 0x01, 0x00}) {
		t.Error("expected false for short payload")
	}
}

func TestHandlePKTEvent_DWSOpen(t *testing.T) {
	c := NewCharmuxClient()

	// Pre-populate a known DWS sensor.
	c.sensors[2] = Sensor{ID: 2, Type: "DWS", Reachable: true}
	c.knownTypes[2] = "DWS"

	// Open: payload ending in 40 00
	pkt := charmux.Event{
		Channel: charmux.ChannelPKT,
		Data:    []byte{0x01, 0x02, 0xF0, 0x00, 0x02, 0x02, 0x00, 0x00, 0x01, 0x55, 0x10, 0x40, 0x00},
	}

	c.handlePKTEvent(pkt)

	s, ok := c.sensors[2]
	if !ok {
		t.Fatal("sensor 2 not found after event")
	}
	if !s.Open {
		t.Error("expected Open=true after DWS open event (40 00)")
	}

	select {
	case ev := <-c.events:
		if ev.SensorID != 2 {
			t.Errorf("event SensorID = %d, want 2", ev.SensorID)
		}
		if !ev.Sensor.Open {
			t.Error("event should have Open=true")
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestHandlePKTEvent_DWSClose(t *testing.T) {
	c := NewCharmuxClient()
	c.sensors[2] = Sensor{ID: 2, Type: "DWS", Open: true, Reachable: true}
	c.knownTypes[2] = "DWS"

	// Close: payload ending in 00 01
	pkt := charmux.Event{
		Channel: charmux.ChannelPKT,
		Data:    []byte{0x01, 0x02, 0xF0, 0x00, 0x02, 0x02, 0x00, 0x00, 0x01, 0x55, 0x10, 0x00, 0x01},
	}

	c.handlePKTEvent(pkt)

	s := c.sensors[2]
	if s.Open {
		t.Error("expected Open=false after DWS close event (00 01)")
	}

	select {
	case ev := <-c.events:
		if ev.Sensor.Open {
			t.Error("event should have Open=false")
		}
	default:
		t.Error("expected event on channel")
	}
}

func TestHandlePKTEvent_KPDHeartbeat(t *testing.T) {
	c := NewCharmuxClient()
	c.sensors[7] = Sensor{ID: 7, Type: "KPD", Reachable: true}

	// KPD heartbeat: 55 09 at payload position.
	pkt := charmux.Event{
		Channel: charmux.ChannelPKT,
		Data:    []byte{0x01, 0x07, 0xF0, 0x00, 0x07, 0x07, 0x00, 0x00, 0x01, 0x55, 0x09},
	}

	c.handlePKTEvent(pkt)

	// Heartbeat should NOT emit an event.
	select {
	case <-c.events:
		t.Error("heartbeat should not emit event")
	default:
		// OK
	}
}

func TestHandlePKTEvent_KPDCommand(t *testing.T) {
	c := NewCharmuxClient()
	c.sensors[7] = Sensor{ID: 7, Type: "KPD", Reachable: true}

	// KPD arm away: real captured format 5501fb8c880f0401
	pkt := charmux.Event{
		Channel: charmux.ChannelPKT,
		Data:    []byte{0x01, 0x07, 0xF0, 0x00, 0x07, 0x07, 0x00, 0x00, 0x01, 0x55, 0x01, 0xfb, 0x8c, 0x88, 0x0f, 0x04, 0x01},
	}

	c.handlePKTEvent(pkt)

	select {
	case ev := <-c.events:
		if ev.SensorID != 7 {
			t.Errorf("event SensorID = %d, want 7", ev.SensorID)
		}
		if ev.Sensor.KPDState != "armed_away" {
			t.Errorf("KPDState = %q, want armed_away", ev.Sensor.KPDState)
		}
	default:
		t.Error("expected event for KPD command")
	}
}

func TestParseKPDState(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		// Real captured: ON button press → armed_away
		// Full PKT: 0114d096c80b148300015501fb8c880f0401
		// payload (data[9:]): 5501fb8c880f0401
		{"arm_away_button", []byte{0x55, 0x01, 0xfb, 0x8c, 0x88, 0x0f, 0x04, 0x01}, "armed_away"},

		// arm_night button press
		{"arm_night_button", []byte{0x55, 0x01, 0xfb, 0x8c, 0x88, 0x0f, 0x04, 0x02}, "armed_night"},

		// Real captured: code 1234 entry → disarmed
		// Full PKT: 0114c0c4c80b148300015501ff8c880f840010320000
		// payload (data[9:]): 5501ff8c880f840010320000
		{"code_entry_disarm", []byte{0x55, 0x01, 0xff, 0x8c, 0x88, 0x0f, 0x84, 0x00, 0x10, 0x32, 0x00, 0x00}, "disarmed"},

		// Unknown button value
		{"unknown_button", []byte{0x55, 0x01, 0x00, 0x00, 0x00, 0x00, 0x04, 0x05}, ""},

		// Too short
		{"too_short", []byte{0x55, 0x01}, ""},

		// Wrong marker
		{"wrong_marker", []byte{0x55, 0x09, 0x00, 0x00, 0x00, 0x00, 0x04, 0x01}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseKPDState(tt.payload)
			if got != tt.want {
				t.Errorf("parseKPDState(%x) = %q, want %q", tt.payload, got, tt.want)
			}
		})
	}
}

func TestHandlePKTEvent_UnknownAddr(t *testing.T) {
	c := NewCharmuxClient()

	// Unknown addr 99 with DWS open payload.
	pkt := charmux.Event{
		Channel: charmux.ChannelPKT,
		Data:    []byte{0x01, 0x63, 0xF0, 0x00, 0x63, 0x63, 0x00, 0x00, 0x01, 0x55, 0x10, 0x00, 0x01},
	}

	c.handlePKTEvent(pkt)

	s, ok := c.sensors[99]
	if !ok {
		t.Fatal("unknown sensor should be auto-created")
	}
	if s.Type != "DWS" {
		t.Errorf("type = %q, want DWS", s.Type)
	}
}

func TestHandlePKTEvent_TooShort(t *testing.T) {
	c := NewCharmuxClient()
	c.handlePKTEvent(charmux.Event{Data: []byte{0x01, 0x02}})
	if len(c.sensors) != 0 {
		t.Error("short packet should be ignored")
	}
}

func TestHandlePKTEvent_BadHeader(t *testing.T) {
	c := NewCharmuxClient()
	// Wrong first byte.
	c.handlePKTEvent(charmux.Event{
		Data: []byte{0x02, 0x02, 0xF0, 0x00, 0x02, 0x02, 0x00, 0x00, 0x01, 0x55, 0x10, 0x00, 0x01},
	})
	if len(c.sensors) != 0 {
		t.Error("bad header should be ignored")
	}
}

func TestNewCharmuxClient_Defaults(t *testing.T) {
	c := NewCharmuxClient()
	if c.mux != nil {
		t.Error("expected nil mux by default")
	}
	if c.sensors == nil {
		t.Error("expected non-nil sensors map")
	}
	if c.events == nil {
		t.Error("expected non-nil events channel")
	}
}

func TestCharmux_Connect_NoMux(t *testing.T) {
	c := NewCharmuxClient()
	err := c.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error when mux is nil")
	}
}

func TestCharmux_ReadSensor_NotFound(t *testing.T) {
	c := NewCharmuxClient()
	_, err := c.ReadSensor(context.Background(), 99, nil)
	if err == nil {
		t.Fatal("expected error for unknown sensor")
	}
}

func TestCharmux_ReadSensor_Found(t *testing.T) {
	c := NewCharmuxClient()
	c.sensors[2] = Sensor{ID: 2, Type: "DWS", Open: true}

	s, err := c.ReadSensor(context.Background(), 2, []string{"state"})
	if err != nil {
		t.Fatalf("ReadSensor: %v", err)
	}
	if s.ID != 2 {
		t.Errorf("ID = %d, want 2", s.ID)
	}
	if !s.Open {
		t.Error("expected Open=true")
	}
}

func TestCharmux_StartPairing_UnknownType(t *testing.T) {
	c := NewCharmuxClient()
	// Need a non-nil mux to pass the first check.
	c.mux = &charmux.Client{}
	_, err := c.StartPairing(context.Background(), "UNKNOWN", "")
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestCharmux_DeleteSensor_NotFound(t *testing.T) {
	// DeleteSensor is idempotent — calling it on an unknown sensor is a no-op
	// that adds the address to the deleted list.
	c := NewCharmuxClient()
	if err := c.DeleteSensor(context.Background(), 99); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.deleted[99] {
		t.Error("expected 99 to be in deleted map")
	}
}

func TestCharmux_DeleteSensor_Found(t *testing.T) {
	c := NewCharmuxClient()
	c.sensors[2] = Sensor{ID: 2, Type: "DWS"}

	err := c.DeleteSensor(context.Background(), 2)
	if err != nil {
		t.Fatalf("DeleteSensor: %v", err)
	}
	if _, ok := c.sensors[2]; ok {
		t.Error("sensor should be removed")
	}
}

func TestCharmux_OpenStream_NotSupported(t *testing.T) {
	c := NewCharmuxClient()
	_, err := c.OpenStream(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported OpenStream")
	}
}

func TestCharmux_Close_Idempotent(t *testing.T) {
	c := NewCharmuxClient()
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second close should not panic.
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCharmux_Events_Channel(t *testing.T) {
	c := NewCharmuxClient()
	ch := c.Events()
	if ch == nil {
		t.Fatal("Events() returned nil")
	}

	// Channel should be readable.
	c.events <- SensorEvent{SensorID: 1}
	select {
	case ev := <-ch:
		if ev.SensorID != 1 {
			t.Errorf("SensorID = %d, want 1", ev.SensorID)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out reading event")
	}
}
