package mqtt

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

func TestBuildDiscoveryTopic_DWS(t *testing.T) {
	s := camera.Sensor{ID: 33, Type: "DWS"}
	topic, ok := buildDiscoveryTopic(s)
	if !ok {
		t.Fatal("expected ok=true for DWS")
	}
	want := "homeassistant/binary_sensor/openqiara_33/config"
	if topic != want {
		t.Errorf("got %q, want %q", topic, want)
	}
}

func TestBuildDiscoveryTopic_PIR(t *testing.T) {
	s := camera.Sensor{ID: 7, Type: "PIR"}
	topic, ok := buildDiscoveryTopic(s)
	if !ok {
		t.Fatal("expected ok=true for PIR")
	}
	want := "homeassistant/binary_sensor/openqiara_7/config"
	if topic != want {
		t.Errorf("got %q, want %q", topic, want)
	}
}

func TestBuildDiscoveryTopic_SRN(t *testing.T) {
	s := camera.Sensor{ID: 1, Type: "SRN"}
	topic, ok := buildDiscoveryTopic(s)
	if !ok {
		t.Fatal("expected ok=true for SRN")
	}
	want := "homeassistant/siren/openqiara_1/config"
	if topic != want {
		t.Errorf("got %q, want %q", topic, want)
	}
}

func TestBuildDiscoveryTopic_KPD_Unsupported(t *testing.T) {
	s := camera.Sensor{ID: 5, Type: "KPD"}
	_, ok := buildDiscoveryTopic(s)
	if ok {
		t.Error("expected ok=false for KPD (events only, no discovery)")
	}
}

func TestBuildDiscoveryPayload_DWS(t *testing.T) {
	s := camera.Sensor{ID: 33, Type: "DWS"}
	p, ok := buildDiscoveryPayload("openqiara", s)
	if !ok {
		t.Fatal("expected ok=true")
	}

	if p.UniqueID != "openqiara_dws_33" {
		t.Errorf("unique_id = %q", p.UniqueID)
	}
	if p.DeviceClass != "opening" {
		t.Errorf("device_class = %q", p.DeviceClass)
	}
	if p.StateTopic != "openqiara/sensor/33/state" {
		t.Errorf("state_topic = %q", p.StateTopic)
	}
	if p.ValueTemplate != "{{ value_json.open | lower if value_json.open is defined else '' }}" {
		t.Errorf("value_template = %q", p.ValueTemplate)
	}
	if p.Device.Manufacturer != "Qiara/Cofidur" {
		t.Errorf("manufacturer = %q", p.Device.Manufacturer)
	}
	if p.Device.Model != "HOMELABDWS" {
		t.Errorf("model = %q, want HOMELABDWS", p.Device.Model)
	}
}

func TestBuildDiscoveryPayload_CustomPrefix(t *testing.T) {
	s := camera.Sensor{ID: 10, Type: "PIR"}
	p, ok := buildDiscoveryPayload("myprefix", s)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if p.StateTopic != "myprefix/sensor/10/state" {
		t.Errorf("state_topic = %q, want myprefix/sensor/10/state", p.StateTopic)
	}
}

func TestMarshalState_DWS(t *testing.T) {
	s := camera.Sensor{ID: 33, Type: "DWS", Open: new(true), Battery: 85, Reachable: true}
	data, err := marshalState(s)
	if err != nil {
		t.Fatal(err)
	}

	var got dwsState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Open == nil || !*got.Open {
		t.Errorf("open = %v, want true", got.Open)
	}
	if got.Battery != 85 {
		t.Errorf("battery = %d, want 85", got.Battery)
	}
	if !got.Reachable {
		t.Error("expected reachable=true")
	}
}

func TestMarshalState_PIR(t *testing.T) {
	s := camera.Sensor{ID: 7, Type: "PIR", Motion: new(true), Battery: 50, Reachable: true}
	data, err := marshalState(s)
	if err != nil {
		t.Fatal(err)
	}

	var got pirState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Motion == nil || !*got.Motion {
		t.Errorf("motion = %v, want true", got.Motion)
	}
}

// TestMarshalState_UnreportedLeavesStateOut: a door or motion sensor not
// heard since the start publishes its battery and reachability, not a
// state: an "open": false would close, in Home Assistant, a door left open.
func TestMarshalState_UnreportedLeavesStateOut(t *testing.T) {
	for _, tc := range []struct {
		typ, field string
	}{{"DWS", "open"}, {"PIR", "motion"}} {
		data, err := marshalState(camera.Sensor{ID: 46, Type: tc.typ, Battery: 100, Reachable: false})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got[tc.field]; ok {
			t.Errorf("%s payload %s: %q published before any report", tc.typ, data, tc.field)
		}
		if got["battery"] != float64(100) || got["reachable"] != false {
			t.Errorf("%s payload %s: want battery 100 and reachable false", tc.typ, data)
		}
	}
}

func TestMarshalState_SRN(t *testing.T) {
	s := camera.Sensor{ID: 1, Type: "SRN", Battery: 100, Reachable: true}
	data, err := marshalState(s)
	if err != nil {
		t.Fatal(err)
	}

	var got srnState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Battery != 100 {
		t.Errorf("battery = %d, want 100", got.Battery)
	}
	if !got.Reachable {
		t.Error("expected reachable=true")
	}
	if got.Active || got.State != "" {
		t.Errorf("active = %v, state = %q: a siren that reported nothing is off", got.Active, got.State)
	}

	// Active while it wails only; an alarm over keeps its state.
	for state, active := range map[string]bool{"alert": true, "alert_over": false, "armed": false} {
		data, _ := marshalState(camera.Sensor{ID: 1, Type: "SRN", SirenState: state})
		var got srnState
		_ = json.Unmarshal(data, &got)
		if got.Active != active || got.State != state {
			t.Errorf("%s: got %+v, want active %v", state, got, active)
		}
	}
}

func TestMarshalState_KPD(t *testing.T) {
	s := camera.Sensor{ID: 5, Type: "KPD", Battery: 70}
	data, err := marshalState(s)
	if err != nil {
		t.Fatal(err)
	}

	var got kpdState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Battery != 70 {
		t.Errorf("battery = %d, want 70", got.Battery)
	}
}

func TestMarshalState_Unknown(t *testing.T) {
	s := camera.Sensor{ID: 99, Type: "XYZ"}
	_, err := marshalState(s)
	if err == nil {
		t.Error("expected error for unknown type")
	}
}

func TestStateTopic(t *testing.T) {
	got := stateTopic("openqiara", 33)
	want := "openqiara/sensor/33/state"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestModelForType(t *testing.T) {
	tests := []struct {
		typ  string
		want string
	}{
		{"DWS", "HOMELABDWS"},
		{"PIR", "HOMELABPIR"},
		{"SRN", "HOMELABSRN"},
		{"KPD", "HOMELABKPD"},
		{"???", "???"},
	}
	for _, tt := range tests {
		if got := modelForType(tt.typ); got != tt.want {
			t.Errorf("modelForType(%q) = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

// TestHAPublisher_ImplementsPublisher checks that HAPublisher satisfies the interface.
func TestHAPublisher_ImplementsPublisher(t *testing.T) {
	var _ Publisher = (*HAPublisher)(nil)
}

// The camera's own entities follow <prefix>/status; the alarm's sensors never
// do (Alarmo would see them unavailable when the broker coughs).
func TestAvailability_CameraEntitiesOnly(t *testing.T) {
	_, shutter := ShutterDiscoveryPayload("openqiara")
	_, iv := IVDetectionDiscoveryPayload("openqiara", "human")
	for name, raw := range map[string][]byte{"shutter": shutter, "iv": iv} {
		var m struct {
			Availability []availabilityItem `json:"availability"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		want := availabilityItem{Topic: "openqiara/status", PayloadAvailable: "online", PayloadNotAvailable: "offline"}
		if len(m.Availability) != 1 || m.Availability[0] != want {
			t.Errorf("%s: availability = %+v, want [%+v]", name, m.Availability, want)
		}
	}

	for _, typ := range []string{"DWS", "PIR", "SRN"} {
		s := camera.Sensor{ID: 1, Type: typ}
		p, _ := buildDiscoveryPayload("openqiara", s)
		if p.Availability != nil {
			t.Errorf("%s: alarm sensor must have no availability", typ)
		}
		for _, e := range buildExtraDiscoveryTopics("openqiara", s) {
			if e.Payload.Availability != nil {
				t.Errorf("%s: extra %s must have no availability", typ, e.Topic)
			}
		}
	}
}

func TestDiscoveryMetadata(t *testing.T) {
	SetCameraVersion("1.2.3")
	t.Cleanup(func() { SetCameraVersion("") })

	// The siren reads its state through state_value_template only.
	srn, _ := buildDiscoveryPayload("openqiara", camera.Sensor{ID: 1, Type: "SRN"})
	if srn.StateValueTemplate == "" || srn.ValueTemplate != "" {
		t.Errorf("siren templates: state=%q value=%q", srn.StateValueTemplate, srn.ValueTemplate)
	}

	// The sensors hang off the camera, whose device carries the version.
	dws, _ := buildDiscoveryPayload("openqiara", camera.Sensor{ID: 2, Type: "DWS"})
	if dws.Device.ViaDevice != cameraDeviceID || cameraDevice().SWVersion != "1.2.3" {
		t.Errorf("via_device=%q sw_version=%q", dws.Device.ViaDevice, cameraDevice().SWVersion)
	}

	// Entities are named by what they are: HA prepends the device name.
	for _, e := range buildExtraDiscoveryTopics("openqiara", camera.Sensor{ID: 2, Type: "DWS", Label: "Porte"}) {
		if n := e.Payload.Name; n != "Batterie" && n != "Température" {
			t.Errorf("extra %s named %q", e.Topic, n)
		}
	}
}

// The temperature entity is created with the sensor's first reading, before
// the state that feeds it, and rediscovered on reconnect even though the
// sensor list taken at the start knows no temperature.
func TestTemperatureEntityCreatedAtFirstReading(t *testing.T) {
	p, c := newRecordingPublisher()
	p.prefix = "openqiara"
	s := camera.Sensor{ID: 20, Type: "PIR"}
	p.sensors = []camera.Sensor{s}
	tempTopic := "homeassistant/sensor/openqiara_20_temperature/config"
	has := func(topic string) bool {
		for _, m := range c.published {
			if m.topic == topic {
				return true
			}
		}
		return false
	}

	_ = p.PublishState(context.Background(), s)
	if has(tempTopic) {
		t.Fatal("temperature entity created without a reading")
	}

	s.Temperature = new(21)
	_ = p.PublishState(context.Background(), s)
	if n := len(c.published); n != 3 || c.published[1].topic != tempTopic {
		t.Fatalf("want entity then state, got %+v", c.published)
	}
	_ = p.PublishState(context.Background(), s) // already known: no new discovery

	c.published = nil
	p.republishDiscovery(context.Background())
	if !has(tempTopic) {
		t.Error("temperature entity not rediscovered on reconnect")
	}
}
