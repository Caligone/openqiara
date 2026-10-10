package camera

// Sensor represents a paired DomusRF sensor.
type Sensor struct {
	ID        int    `json:"id"`
	Type      string `json:"type"`      // "DWS", "PIR", "SRN", "KPD"
	TypeName  string `json:"type_name"` // e.g. "Node.DomusNode.HlDws"
	ItemID    string `json:"item_id"`   // hardware identifier
	Battery   int    `json:"battery"`   // percent, as fbxhome showed it
	Reachable bool   `json:"reachable"`
	// Temperature in °C, nil until the sensor sent one.
	Temperature *int `json:"temperature,omitempty"`
	Open        bool `json:"open"`   // DWS: door/window open
	Motion      bool `json:"motion"` // PIR: motion detected
	// Reported: Open or Motion came from the sensor since openqiarad
	// started. Until then they hold their zero value, not a state.
	Reported bool `json:"-"`
	// No tamper field: DomusRF sensors expose no usable tamper state (issue #30).
	KPDState string `json:"kpd_state,omitempty"` // KPD: "disarmed", "armed_away", "armed_night"
	// SirenState is what the siren reports: off, test, exit_delay, armed,
	// entry_delay, alert, alert_over (alert ended, still set off).
	SirenState string `json:"siren_state,omitempty"`
	LastSeen   int64  `json:"last_seen"`
	Label      string `json:"label,omitempty"` // user-defined name
}

// StateKnown tells whether the sensor has a state to publish. A door or
// motion sensor has none until it reports after a start: its zero value
// would overwrite, as closed, the state Home Assistant kept.
func (s Sensor) StateKnown() bool {
	return s.Reported || (s.Type != "DWS" && s.Type != "PIR")
}

// SensorEvent is emitted when a sensor state changes.
type SensorEvent struct {
	SensorID int
	Sensor   Sensor
	// SirenReport: the siren just reported its state (Sensor.SirenState);
	// other events of a siren carry its last report.
	SirenReport bool
}

// NodeType maps sensor short types to the model prefix of their pairing
// beacon (e.g. HOMELABDWS00ACFD).
var NodeType = map[string]string{
	"DWS": "HOMELABDWS",
	"PIR": "HOMELABPIR",
	"SRN": "HOMELABSRN",
	"KPD": "HOMELABKPD",
}
