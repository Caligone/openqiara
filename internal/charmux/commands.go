package charmux

import (
	"context"
	"fmt"
)

// CTRL command opcodes.
const (
	OpGetInfo          = 0x02
	OpGetNet           = 0x05
	OpGetNodes         = 0x07
	OpStartPairRelay   = 0x15
	OpStartPairInternal = 0x13
)

// GetInfo sends GET_INFO and parses the 8-byte response into MCUInfo.
func (c *Client) GetInfo(ctx context.Context) (*MCUInfo, error) {
	resp, err := c.SendCTRL(ctx, []byte{OpGetInfo})
	if err != nil {
		return nil, err
	}
	if len(resp) < 8 {
		return nil, fmt.Errorf("charmux: GetInfo: expected 8 bytes, got %d", len(resp))
	}
	// Response: [0x02, netid_lo, netid_hi, addr, flag0, flag1, flag2, state]
	info := &MCUInfo{
		NetworkID: uint16(resp[1]) | uint16(resp[2])<<8,
		Address:   resp[3],
		Flags:     [3]byte{resp[4], resp[5], resp[6]},
		State:     resp[7],
	}
	return info, nil
}

// GetNet sends GET_NET and returns the raw 1-byte response.
func (c *Client) GetNet(ctx context.Context) (byte, error) {
	resp, err := c.SendCTRL(ctx, []byte{OpGetNet})
	if err != nil {
		return 0, err
	}
	if len(resp) < 1 {
		return 0, fmt.Errorf("charmux: GetNet: empty response")
	}
	return resp[0], nil
}

// GetNodes sends GET_NODES and returns the raw 74-byte node table.
func (c *Client) GetNodes(ctx context.Context) ([]byte, error) {
	resp, err := c.SendCTRL(ctx, []byte{OpGetNodes})
	if err != nil {
		return nil, err
	}
	if len(resp) < 2 {
		return resp, nil // Short response — let caller handle
	}
	return resp, nil
}

// PairingMode selects relay or internal pairing.
type PairingMode byte

const (
	PairingRelay    PairingMode = OpStartPairRelay
	PairingInternal PairingMode = OpStartPairInternal
)

// WriteConfigZone1 writes 32 bytes to MCU config zone 1 (vendor key).
// Response opcode: 0x04.
func (c *Client) WriteConfigZone1(ctx context.Context, data [32]byte) error {
	cmd := make([]byte, 33)
	cmd[0] = 0x01
	copy(cmd[1:], data[:])
	_, err := c.SendCTRL(ctx, cmd)
	return err
}

// StartPairingRelay sends START_PAIRING (0x15) in relay mode.
func (c *Client) StartPairingRelay(ctx context.Context) ([]byte, error) {
	cmd := make([]byte, 18)
	cmd[0] = OpStartPairRelay
	cmd[1] = 0x01
	cmd[5] = 0x01
	return c.SendCTRL(ctx, cmd)
}

// StartPairingInternal sends START_PAIRING (0x13) in internal mode.
// The vendor key must be written first via WriteConfigZone1.
// Response opcode: 0x14.
func (c *Client) StartPairingInternal(ctx context.Context) ([]byte, error) {
	return c.SendCTRL(ctx, []byte{OpStartPairInternal})
}

// StopPairing sends opcode 0x16 to stop pairing mode.
func (c *Client) StopPairing(ctx context.Context) error {
	_, err := c.SendCTRL(ctx, []byte{0x16})
	return err
}
