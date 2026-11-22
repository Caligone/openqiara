package mqtt

// discoveryPayload is the HA MQTT auto-discovery config message.
type discoveryPayload struct {
	Name                   string `json:"name"`
	UniqueID               string `json:"unique_id"`
	DeviceClass            string `json:"device_class,omitempty"`
	StateTopic             string `json:"state_topic"`
	CommandTopic           string `json:"command_topic,omitempty"`
	ValueTemplate          string `json:"value_template,omitempty"`
	StateValueTemplate     string `json:"state_value_template,omitempty"` // siren: HA's MQTT siren has no value_template
	PayloadOn              string `json:"payload_on,omitempty"`
	PayloadOff             string `json:"payload_off,omitempty"`
	JSONAttributesTopic    string `json:"json_attributes_topic,omitempty"`
	JSONAttributesTemplate string `json:"json_attributes_template,omitempty"`
	UnitOfMeasurement      string `json:"unit_of_measurement,omitempty"`
	EntityCategory         string `json:"entity_category,omitempty"`
	// Availability is left out for the alarm's sensors: a camera dropping off
	// the network must not turn them "unavailable" under Alarmo. Set on the
	// camera's own entities only.
	Availability []availabilityItem `json:"availability,omitempty"`
	Device       haDevice           `json:"device"`

	// Siren: HA offers volume and duration unless told not to, and both
	// would be ignored (the alert's level is set by the arming frame).
	SupportVolumeSet *bool `json:"support_volume_set,omitempty"`
	SupportDuration  *bool `json:"support_duration,omitempty"`
}

// availabilityItem points an entity at the topic that says whether the camera
// is on the broker.
type availabilityItem struct {
	Topic               string `json:"topic"`
	PayloadAvailable    string `json:"payload_available"`
	PayloadNotAvailable string `json:"payload_not_available"`
}

// haDevice is the device block in HA discovery config.
type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
	// Left out when empty, so that the payloads of the existing entities
	// stay as they were.
	SWVersion string `json:"sw_version,omitempty"`
	ViaDevice string `json:"via_device,omitempty"`
}

// dwsState is the MQTT state payload for a door/window sensor.
type dwsState struct {
	Open        *bool `json:"open,omitempty"` // absent until the sensor reports after a start
	Battery     int   `json:"battery"`
	Reachable   bool  `json:"reachable"`
	Temperature *int  `json:"temperature,omitempty"` // °C, absent until reported
}

// pirState is the MQTT state payload for a motion sensor.
type pirState struct {
	Motion      *bool `json:"motion,omitempty"` // absent until the sensor reports after a start
	Battery     int   `json:"battery"`
	Reachable   bool  `json:"reachable"`
	Temperature *int  `json:"temperature,omitempty"` // °C, absent until reported
}

// srnState is the MQTT state payload for a siren.
type srnState struct {
	Active      bool   `json:"active"`          // wailing
	State       string `json:"state,omitempty"` // as the siren reports it (camera.Sensor.SirenState)
	Battery     int    `json:"battery"`
	Reachable   bool   `json:"reachable"`
	Temperature *int   `json:"temperature,omitempty"` // °C, absent until reported
}

// kpdState is the MQTT state payload for a keypad event.
type kpdState struct {
	Battery     int  `json:"battery"`
	Reachable   bool `json:"reachable"`
	Temperature *int `json:"temperature,omitempty"` // °C, absent until reported
}
