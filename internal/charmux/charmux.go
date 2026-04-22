// Package charmux provides a UDP client for direct communication with the
// Qiara MCU via charmux socket pairs.
package charmux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	defaultHost        = "127.0.0.1"
	defaultReadTimeout = 3 * time.Second
	maxUDPPayload      = 512
	eventChanSize      = 64
)

type Option func(*Client)

func WithHost(host string) Option   { return func(c *Client) { c.host = host } }
func WithReadTimeout(d time.Duration) Option { return func(c *Client) { c.readTimeout = d } }
func WithLogger(l *slog.Logger) Option { return func(c *Client) { c.log = l } }

type Client struct {
	host        string
	readTimeout time.Duration
	log         *slog.Logger

	ctrlConn    *net.UDPConn
	pktConn     *net.UDPConn
	shutterConn *net.UDPConn
	events chan Event
	done   chan struct{}
	wg     sync.WaitGroup

	// CTRL request/response: caller sends request, waits on ctrlResp
	ctrlMu   sync.Mutex
	ctrlReq  chan []byte   // send a CTRL command
	ctrlResp chan []byte   // receive the matching response
}

func New(opts ...Option) *Client {
	c := &Client{
		host:        defaultHost,
		readTimeout: defaultReadTimeout,
		log:         slog.Default(),
		events:      make(chan Event, eventChanSize),
		done:        make(chan struct{}),
		ctrlReq:     make(chan []byte),
		ctrlResp:    make(chan []byte, 1),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) Connect(ctx context.Context) error {
	var err error
	c.ctrlConn, err = dialChannel(c.host, ChannelCTRL)
	if err != nil {
		return fmt.Errorf("charmux: connect CTRL: %w", err)
	}
	c.pktConn, err = dialChannel(c.host, ChannelPKT)
	if err != nil {
		c.ctrlConn.Close()
		return fmt.Errorf("charmux: connect PKT: %w", err)
	}
	// Shutter: bind client port 8007, connect to server 8006.
	// hlcamd may also use 8007 — if bind fails, try unbound.
	c.shutterConn, err = dialChannel(c.host, ChannelShutter)
	if err != nil {
		// Fallback: unbound socket (may not work if 8006 rejects unknown sources)
		shutterAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", c.host, defaultPorts[ChannelShutter][1]))
		c.shutterConn, err = net.DialUDP("udp4", nil, shutterAddr)
		if err != nil {
			c.log.Warn("charmux: Shutter channel unavailable", "err", err)
		}
	}

	// Watchdog channel: send 0x05 to 8004 (captured from fbxhome init)
	// Don't bind 8005 (watchdog_mcu process owns it) — use unbound socket
	wdAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", c.host, 8004))
	if err == nil {
		wdConn, wdErr := net.DialUDP("udp4", nil, wdAddr)
		if wdErr == nil {
			wdConn.Write([]byte{0x05})
			wdConn.Close()
			c.log.Info("charmux: watchdog init sent (0x05)")
		} else {
			c.log.Warn("charmux: watchdog init failed", "err", wdErr)
		}
	}

	// Shutter init byte 0x02 (captured from fbxhome)
	if c.shutterConn != nil {
		c.shutterConn.Write([]byte{0x02})
		c.log.Info("charmux: shutter init sent (0x02)")
	}

	c.log.Info("charmux: connected", "ctrl", c.ctrlConn.LocalAddr(), "pkt", c.pktConn.LocalAddr())

	c.wg.Add(2)
	go c.recvPKTLoop()
	go c.recvCTRLLoop()

	return nil
}

func dialChannel(host string, ch int) (*net.UDPConn, error) {
	ports := defaultPorts[ch]
	localAddr := &net.UDPAddr{IP: net.ParseIP(host), Port: ports[0]}
	remoteAddr := &net.UDPAddr{IP: net.ParseIP(host), Port: ports[1]}
	return net.DialUDP("udp4", localAddr, remoteAddr)
}

// responseOpcode maps request opcodes to their expected response opcodes.
// Most opcodes echo themselves, but some have different response codes.
var responseOpcode = map[byte]byte{
	0x01: 0x04, // Config write zone 1
	0x13: 0x14, // Internal pairing
	0x0f: 0x10, // Config write 4B
	0x1c: 0x1d, // NVM write 4B
	0x1e: 0x16, // NVM write 32B
}

// SendCTRL sends a command on CTRL and waits for a response matching the expected opcode.
func (c *Client) SendCTRL(ctx context.Context, data []byte) ([]byte, error) {
	c.ctrlMu.Lock()
	defer c.ctrlMu.Unlock()

	if _, err := c.ctrlConn.Write(data); err != nil {
		return nil, fmt.Errorf("charmux: write CTRL: %w", err)
	}

	expectedOp := data[0]
	if mapped, ok := responseOpcode[data[0]]; ok {
		expectedOp = mapped
	}
	deadline := time.After(c.readTimeout)
	if d, ok := ctx.Deadline(); ok {
		deadline = time.After(time.Until(d))
	}

	for {
		select {
		case resp := <-c.ctrlResp:
			if len(resp) > 0 && (resp[0] == expectedOp || resp[0] == expectedOp|0x80) {
				return resp, nil
			}
			// Not our opcode — put it in events
			select {
			case c.events <- Event{Channel: ChannelCTRL, Data: resp}:
			default:
			}
		case <-deadline:
			return nil, fmt.Errorf("charmux: CTRL timeout waiting for 0x%02x", expectedOp)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// SendRawCTRL sends data on CTRL without waiting for response.
func (c *Client) SendRawCTRL(data []byte) error {
	_, err := c.ctrlConn.Write(data)
	return err
}

// StartCTRLListener is a no-op — the CTRL listener runs permanently now.
func (c *Client) StartCTRLListener() {}
func (c *Client) StopCTRLListener()  {}

// SendPKT sends raw data on the PKT UDP channel (→ UART channel 1).
func (c *Client) SendPKT(_ context.Context, data []byte) error {
	_, err := c.pktConn.Write(data)
	return err
}

// SendWatchdog sends 0x05 on the watchdog channel (8005→8004).
// This may trigger NVM commit on the MCU for pairing persistence.
func (c *Client) SendWatchdog() {
	wdAddr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", c.host, 8004))
	if err != nil {
		return
	}
	conn, err := net.DialUDP("udp4", nil, wdAddr)
	if err != nil {
		return
	}
	conn.Write([]byte{0x05})
	conn.Close()
}

// SendShutter sends a command on the Shutter channel.
// 0x01 = open, 0x02 = close.
func (c *Client) SendShutter(open bool) error {
	if c.shutterConn == nil {
		return fmt.Errorf("shutter channel not connected")
	}
	cmd := byte(0x02) // close
	if open {
		cmd = 0x01
	}
	_, err := c.shutterConn.Write([]byte{cmd})
	return err
}

// Events returns the channel for async events (PKT + unsolicited CTRL).
func (c *Client) Events() chan Event { return c.events }

// CTRLResp returns the channel where CTRL responses are buffered.
// Used by the pairing code to drain responses that SendCTRL would normally read.
func (c *Client) CTRLResp() <-chan []byte { return c.ctrlResp }

func (c *Client) Close() error {
	close(c.done)
	if c.ctrlConn != nil {
		c.ctrlConn.Close()
	}
	if c.pktConn != nil {
		c.pktConn.Close()
	}
	if c.shutterConn != nil {
		c.shutterConn.Close()
	}
	c.wg.Wait()
	close(c.events)
	return nil
}

// recvCTRLLoop is the SINGLE reader for the CTRL socket.
// It dispatches messages to either ctrlResp (if SendCTRL is waiting) or events.
func (c *Client) recvCTRLLoop() {
	defer c.wg.Done()
	buf := make([]byte, maxUDPPayload)

	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.ctrlConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := c.ctrlConn.Read(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			select {
			case <-c.done:
				return
			default:
				continue
			}
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		// Try to deliver to SendCTRL first (non-blocking)
		select {
		case c.ctrlResp <- data:
		default:
			// SendCTRL not waiting — deliver as event
			select {
			case c.events <- Event{Channel: ChannelCTRL, Data: data}:
			default:
				c.log.Warn("charmux: event channel full, dropping CTRL event")
			}
		}
	}
}

func (c *Client) recvPKTLoop() {
	defer c.wg.Done()
	buf := make([]byte, maxUDPPayload)

	for {
		select {
		case <-c.done:
			return
		default:
		}

		c.pktConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, err := c.pktConn.Read(buf)
		if err != nil {
			if isTimeout(err) {
				continue
			}
			select {
			case <-c.done:
				return
			default:
				c.log.Warn("charmux: PKT read error", "err", err)
				continue
			}
		}

		data := make([]byte, n)
		copy(data, buf[:n])

		select {
		case c.events <- Event{Channel: ChannelPKT, Data: data}:
		default:
			c.log.Warn("charmux: event channel full, dropping PKT event")
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
