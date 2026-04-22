package mqtt

import (
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
	if p.ValueTemplate != "{{ value_json.open | lower }}" {
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
	s := camera.Sensor{ID: 33, Type: "DWS", Open: true, Battery: 85, Temperature: 215, Reachable: true}
	data, err := marshalState(s)
	if err != nil {
		t.Fatal(err)
	}

	var got dwsState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Open {
		t.Error("expected open=true")
	}
	if got.Battery != 85 {
		t.Errorf("battery = %d, want 85", got.Battery)
	}
	if got.Temperature != 21.5 {
		t.Errorf("temperature = %f, want 21.5", got.Temperature)
	}
	if !got.Reachable {
		t.Error("expected reachable=true")
	}
}

func TestMarshalState_PIR(t *testing.T) {
	s := camera.Sensor{ID: 7, Type: "PIR", Motion: true, Battery: 50, Temperature: 200, Reachable: true}
	data, err := marshalState(s)
	if err != nil {
		t.Fatal(err)
	}

	var got pirState
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.Motion {
		t.Error("expected motion=true")
	}
	if got.Temperature != 20.0 {
		t.Errorf("temperature = %f, want 20.0", got.Temperature)
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
