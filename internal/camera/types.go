package camera

// Sensor represents a paired DomusRF sensor.
type Sensor struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`        // "DWS", "PIR", "SRN", "KPD"
	TypeName    string `json:"type_name"`   // e.g. "Node.DomusNode.HlDws"
	ItemID      string `json:"item_id"`     // hardware identifier
	Battery     int    `json:"battery"`     // percentage
	Temperature int    `json:"temperature"` // value from API (divide by 10 for Celsius)
	Reachable   bool   `json:"reachable"`
	Open        bool   `json:"open"`             // DWS: door/window open
	Motion      bool   `json:"motion"`           // PIR: motion detected
	Tamper      bool   `json:"tamper"`           // tamper switch
	KPDState    string `json:"kpd_state,omitempty"` // KPD: "disarmed", "armed_away", "armed_night"
	LastSeen    int64  `json:"last_seen"`
	Label       string `json:"label,omitempty"` // user-defined name
}

// SensorEvent is emitted when a sensor state changes.
type SensorEvent struct {
	SensorID int
	Sensor   Sensor
}

// StreamInfo contains video stream connection details.
type StreamInfo struct {
	Passphrase string `json:"passphrase"`
	Port       int    `json:"port"`
}

// NodeType maps sensor short types to fbxhome pairing node types.
var NodeType = map[string]string{
	"DWS": "HOMELABDWS",
	"PIR": "HOMELABPIR",
	"SRN": "HOMELABSRN",
	"KPD": "HOMELABKPD",
}

// AdapterType maps sensor short types to fbxhome adapter types.
var AdapterType = map[string]string{
	"DWS": "Adapter.DomusAdapter",
	"PIR": "Adapter.DomusAdapter",
	"SRN": "Adapter.DomusAdapter",
	"KPD": "Adapter.DomusAdapter",
}

// sensorTypeFromNodeType maps fbxhome type_name to short type.
var sensorTypeFromNodeType = map[string]string{
	"Node.DomusNode.HlDws": "DWS",
	"Node.DomusNode.HlPir": "PIR",
	"Node.DomusNode.HlSrn": "SRN",
	"Node.DomusNode.HLKpd": "KPD",
}

// EndpointValue is a public type for raw endpoint read results.
type EndpointValue struct {
	EPName string      `json:"ep_name"`
	Value  interface{} `json:"value"`
}

// EndpointWriteEntry is a public type for raw endpoint write commands.
type EndpointWriteEntry struct {
	EPName string      `json:"ep_name"`
	Value  interface{} `json:"value"`
}

// --- fbxhome API request/response types ---

// domusNodesResponse is the response from GET /rpc/get_domus_nodes.
type domusNodesResponse struct {
	Result []domusNode `json:"result"`
}

type domusNode struct {
	ID        int            `json:"id"`
	TypeName  string         `json:"type_name"`
	AdapterID int            `json:"adapter_id"`
	Values    domusValues    `json:"values"`
	ItemID    string         `json:"item_id"`
}

type domusValues struct {
	Reachable   int            `json:"reachable"`
	Battery     int            `json:"battery"`
	Temperature temperatureVal `json:"temperature"`
	RFRSSI      int            `json:"rf_rssi"`
}

type temperatureVal struct {
	Value     int   `json:"value"`
	Timestamp int64 `json:"timestamp"`
}

// endpointsReadRequest is the body for POST /api/v1/home/endpoints_read.
type endpointsReadRequest struct {
	List []endpointQuery `json:"list"`
}

type endpointQuery struct {
	NodeID    int      `json:"node_id"`
	Endpoints []string `json:"eps"`
}

// endpointsReadResponse is the response from endpoints_read.
type endpointsReadResponse struct {
	List []endpointResult `json:"list"`
}

type endpointResult struct {
	NodeID   int             `json:"node_id"`
	EPValues []endpointValue `json:"ep_values"`
}

type endpointValue struct {
	EPName string      `json:"ep_name"`
	Value  interface{} `json:"value"`
}

// endpointsWriteRequest is the body for POST /api/v1/home/endpoints_write.
type endpointsWriteRequest struct {
	List []endpointWriteQuery `json:"list"`
}

type endpointWriteQuery struct {
	NodeID    int              `json:"node_id"`
	Endpoints []endpointWrite `json:"eps"`
}

type endpointWrite struct {
	EPName string      `json:"ep_name"`
	Value  interface{} `json:"value"`
}

// deleteRequest is the body for POST /api/v1/home/delete.
type deleteRequest struct {
	Op   string `json:"op"`
	List []int  `json:"list"`
}

// pairingRequest is the body for POST /api/v1/home/pairing.
type pairingRequest struct {
	Op          string   `json:"op"`
	NodeType    string   `json:"node_type,omitempty"`
	AdapterType string   `json:"adapter_type,omitempty"`
	Session     int      `json:"session,omitempty"`
	Fields      []string `json:"fields,omitempty"`
}

// pairingResponse is the response from pairing operations.
type pairingResponse struct {
	Session     int    `json:"session,omitempty"`
	LayoutName  string `json:"layout_name,omitempty"`
	PageID      int    `json:"page_id,omitempty"`
	Refresh     int    `json:"refresh,omitempty"`
	Last        int    `json:"last,omitempty"`
	NodeID      int    `json:"node_id,omitempty"`
	NodeType    string `json:"node_type,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}
