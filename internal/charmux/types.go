package charmux

// Channel indices for charmux UDP socket pairs.
const (
	ChannelCTRL     = 0
	ChannelPKT      = 1
	ChannelShutter  = 2
	ChannelWatchdog = 3
)

// Default UDP port pairs: [clientPort, serverPort].
var defaultPorts = map[int][2]int{
	ChannelCTRL:     {8001, 8000},
	ChannelPKT:      {8003, 8002},
	ChannelShutter:  {8007, 8006},
	ChannelWatchdog: {8005, 8004},
}

// MCUInfo holds the parsed response from GET_INFO (0x02).
type MCUInfo struct {
	NetworkID uint16
	Address   uint8
	Flags     [3]byte
	State     uint8
}

// Event represents an asynchronous message received on a channel.
type Event struct {
	Channel int
	Data    []byte
}
