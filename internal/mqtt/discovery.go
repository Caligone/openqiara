package mqtt

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caligone/openqiara/internal/camera"
)

// sensorMeta describes how a sensor type maps to HA auto-discovery.
type sensorMeta struct {
	domain      string // HA domain: "binary_sensor", "siren"
	deviceClass string // HA device_class or ""
	stateField  string // JSON field used in value_template
}

var sensorTypeMap = map[string]sensorMeta{
	"DWS": {domain: "binary_sensor", deviceClass: "opening", stateField: "open"},
	"PIR": {domain: "binary_sensor", deviceClass: "motion", stateField: "motion"},
	"SRN": {domain: "siren", deviceClass: "", stateField: "active"},
}

func sensorDisplayName(sensor camera.Sensor) string {
	if sensor.Label != "" {
		return sensor.Label
	}
	return fmt.Sprintf("OpenQiara %s %d", sensor.Type, sensor.ID)
}

func sensorDevice(sensor camera.Sensor) haDevice {
	return haDevice{
		Identifiers:  []string{fmt.Sprintf("openqiara_%d", sensor.ID)},
		Name:         sensorDisplayName(sensor),
		Manufacturer: "Qiara/Cofidur",
		Model:        modelForType(sensor.Type),
	}
}

// buildDiscoveryTopic returns the HA auto-discovery config topic for a sensor.
func buildDiscoveryTopic(sensor camera.Sensor) (string, bool) {
	meta, ok := sensorTypeMap[sensor.Type]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("homeassistant/%s/openqiara_%d/config", meta.domain, sensor.ID), true
}

// buildDiscoveryPayload returns the HA auto-discovery JSON payload for a sensor.
func buildDiscoveryPayload(prefix string, sensor camera.Sensor) (discoveryPayload, bool) {
	meta, ok := sensorTypeMap[sensor.Type]
	if !ok {
		return discoveryPayload{}, false
	}

	typeLower := strings.ToLower(sensor.Type)
	uniqueID := fmt.Sprintf("openqiara_%s_%d", typeLower, sensor.ID)
	name := sensorDisplayName(sensor)
	st := stateTopic(prefix, sensor.ID)

	p := discoveryPayload{
		Name:                   name,
		UniqueID:               uniqueID,
		DeviceClass:            meta.deviceClass,
		StateTopic:             st,
		ValueTemplate:          fmt.Sprintf("{{ value_json.%s | lower }}", meta.stateField),
		PayloadOn:              "true",
		PayloadOff:             "false",
		JSONAttributesTopic:    st,
		JSONAttributesTemplate: `{"battery": {{ value_json.battery }}, "temperature": {{ value_json.temperature }}, "reachable": {{ value_json.reachable | lower }}}`,
		Device:                 sensorDevice(sensor),
	}

	if sensor.Type == "SRN" {
		p.CommandTopic = fmt.Sprintf("%s/siren/%d/set", prefix, sensor.ID)
		p.ValueTemplate = "{{ value_json.active | lower }}"
		p.PayloadOn = "true"
		p.PayloadOff = "false"
	}

	return p, true
}

// ExtraDiscovery holds a topic and its JSON payload bytes.
type ExtraDiscovery struct {
	Topic   string
	Payload []byte
}

// BuildExtraDiscoveryTopics returns additional HA discovery configs for battery and temperature, JSON-encoded.
func BuildExtraDiscoveryTopics(prefix string, sensor camera.Sensor) []ExtraDiscovery {
	extras := buildExtraDiscoveryTopics(prefix, sensor)
	result := make([]ExtraDiscovery, 0, len(extras))
	for _, e := range extras {
		data, err := json.Marshal(e.Payload)
		if err != nil {
			continue
		}
		result = append(result, ExtraDiscovery{Topic: e.Topic, Payload: data})
	}
	return result
}

// buildExtraDiscoveryTopics returns additional HA discovery configs for battery and temperature.
func buildExtraDiscoveryTopics(prefix string, sensor camera.Sensor) []struct {
	Topic   string
	Payload discoveryPayload
} {
	st := stateTopic(prefix, sensor.ID)
	device := sensorDevice(sensor)
	name := sensorDisplayName(sensor)
	typeLower := strings.ToLower(sensor.Type)

	var extras []struct {
		Topic   string
		Payload discoveryPayload
	}

	// Battery sensor
	extras = append(extras, struct {
		Topic   string
		Payload discoveryPayload
	}{
		Topic: fmt.Sprintf("homeassistant/sensor/openqiara_%d_battery/config", sensor.ID),
		Payload: discoveryPayload{
			Name:                name + " Batterie",
			UniqueID:            fmt.Sprintf("openqiara_%s_%d_battery", typeLower, sensor.ID),
			DeviceClass:         "battery",
			StateTopic:          st,
			ValueTemplate:       "{{ value_json.battery }}",
			JSONAttributesTopic: st,
			Device:              device,
		},
	})

	// Temperature sensor
	extras = append(extras, struct {
		Topic   string
		Payload discoveryPayload
	}{
		Topic: fmt.Sprintf("homeassistant/sensor/openqiara_%d_temperature/config", sensor.ID),
		Payload: discoveryPayload{
			Name:                name + " Température",
			UniqueID:            fmt.Sprintf("openqiara_%s_%d_temperature", typeLower, sensor.ID),
			DeviceClass:         "temperature",
			StateTopic:          st,
			ValueTemplate:       "{{ value_json.temperature }}",
			JSONAttributesTopic: st,
			Device:              device,
		},
	})

	return extras
}

func modelForType(sensorType string) string {
	models := map[string]string{
		"DWS": "HOMELABDWS",
		"PIR": "HOMELABPIR",
		"SRN": "HOMELABSRN",
		"KPD": "HOMELABKPD",
	}
	if m, ok := models[sensorType]; ok {
		return m
	}
	return sensorType
}

func stateTopic(prefix string, sensorID int) string {
	return fmt.Sprintf("%s/sensor/%d/state", prefix, sensorID)
}

// CameraDiscoveryPayload is not used — HLS streams should be added
// as generic camera in HA with URL: http://<camera_ip>:8080/stream/HLS_TEST.m3u8

// ShutterDiscoveryPayload returns the HA auto-discovery config for the camera shutter as a switch.
func ShutterDiscoveryPayload(prefix string) (string, []byte) {
	topic := "homeassistant/switch/openqiara_shutter/config"
	payload := discoveryPayload{
		Name:          "OpenQiara Shutter",
		UniqueID:      "openqiara_shutter",
		StateTopic:    prefix + "/shutter/state",
		CommandTopic:  prefix + "/shutter/set",
		ValueTemplate: "{{ value_json.state }}",
		PayloadOn:     "open",
		PayloadOff:    "closed",
		Device: haDevice{
			Identifiers:  []string{"openqiara_camera"},
			Name:         "OpenQiara Caméra",
			Manufacturer: "Qiara/Cofidur",
			Model:        "HOMELABCAM",
		},
	}
	data, _ := json.Marshal(payload)
	return topic, data
}

// IVDetectionDiscoveryPayload retourne la config auto-discovery HA pour
// un binary_sensor de détection IntelliVision (humain ou animal).
//
// kind est "human" ou "pet" — utilisé pour le suffixe topic + unique_id
// + nom convivial + device_class HA (motion/occupancy).
func IVDetectionDiscoveryPayload(prefix, kind string) (string, []byte) {
	displayName := "Humain détecté"
	deviceClass := "motion"
	if kind == "pet" {
		displayName = "Animal détecté"
		deviceClass = "occupancy"
	}
	topic := "homeassistant/binary_sensor/openqiara_iv_" + kind + "/config"
	payload := discoveryPayload{
		Name:          displayName,
		UniqueID:      "openqiara_iv_" + kind,
		DeviceClass:   deviceClass,
		StateTopic:    prefix + "/iv/" + kind + "/state",
		ValueTemplate: "{{ value_json.detected | lower }}",
		PayloadOn:     "true",
		PayloadOff:    "false",
		JSONAttributesTopic:    prefix + "/iv/" + kind + "/state",
		JSONAttributesTemplate: "{\"confidence\": {{ value_json.confidence }}, \"object_id\": \"{{ value_json.object_id }}\"}",
		Device: haDevice{
			Identifiers:  []string{"openqiara_camera"},
			Name:         "OpenQiara Caméra",
			Manufacturer: "Qiara/Cofidur",
			Model:        "HOMELABCAM",
		},
	}
	data, _ := json.Marshal(payload)
	return topic, data
}
