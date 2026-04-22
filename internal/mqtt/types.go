package mqtt

// discoveryPayload is the HA MQTT auto-discovery config message.
type discoveryPayload struct {
	Name                   string   `json:"name"`
	UniqueID               string   `json:"unique_id"`
	DeviceClass            string   `json:"device_class,omitempty"`
	StateTopic             string   `json:"state_topic"`
	CommandTopic           string   `json:"command_topic,omitempty"`
	ValueTemplate          string   `json:"value_template"`
	PayloadOn              string   `json:"payload_on,omitempty"`
	PayloadOff             string   `json:"payload_off,omitempty"`
	JSONAttributesTopic    string   `json:"json_attributes_topic,omitempty"`
	JSONAttributesTemplate string   `json:"json_attributes_template,omitempty"`
	UnitOfMeasurement      string   `json:"unit_of_measurement,omitempty"`
	EntityCategory         string   `json:"entity_category,omitempty"`
	Device                 haDevice `json:"device"`
}

// haDevice is the device block in HA discovery config.
type haDevice struct {
	Identifiers  []string `json:"identifiers"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Model        string   `json:"model"`
}

// dwsState is the MQTT state payload for a door/window sensor.
type dwsState struct {
	Open        bool    `json:"open"`
	Battery     int     `json:"battery"`
	Temperature float64 `json:"temperature"`
	Reachable   bool    `json:"reachable"`
}

// pirState is the MQTT state payload for a motion sensor.
type pirState struct {
	Motion      bool    `json:"motion"`
	Battery     int     `json:"battery"`
	Temperature float64 `json:"temperature"`
	Reachable   bool    `json:"reachable"`
}

// srnState is the MQTT state payload for a siren.
type srnState struct {
	Active    bool `json:"active"`
	Battery   int  `json:"battery"`
	Reachable bool `json:"reachable"`
}

// kpdState is the MQTT state payload for a keypad event.
type kpdState struct {
	Battery   int  `json:"battery"`
	Reachable bool `json:"reachable"`
}
