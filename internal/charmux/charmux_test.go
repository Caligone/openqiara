package charmux

import (
	"context"
	"net"
	"testing"
	"time"
)

// mockServer simulates a charmux UDP endpoint.
// It binds the server-side port and responds to the client.
type mockServer struct {
	conn *net.UDPConn
}

func newMockServer(t *testing.T, port int) *mockServer {
	t.Helper()
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		t.Fatalf("mock server listen on :%d: %v", port, err)
	}
	return &mockServer{conn: conn}
}

func (s *mockServer) close() { s.conn.Close() }

// respond reads one packet and sends reply back.
func (s *mockServer) respond(reply []byte) error {
	buf := make([]byte, 256)
	s.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, remote, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		return err
	}
	_, err = s.conn.WriteToUDP(reply, remote)
	return err
}

// pickFreePorts finds two consecutive free UDP ports for a test.
// Returns (clientPort, serverPort).
func pickFreePorts(t *testing.T) (int, int) {
	t.Helper()
	// Bind two ephemeral ports and release them.
	c1, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p1 := c1.LocalAddr().(*net.UDPAddr).Port
	p2 := c2.LocalAddr().(*net.UDPAddr).Port
	c1.Close()
	c2.Close()
	return p1, p2
}

func TestGetInfo(t *testing.T) {
	clientPort, serverPort := pickFreePorts(t)

	// Override default ports for the test.
	origPorts := defaultPorts[ChannelCTRL]
	defaultPorts[ChannelCTRL] = [2]int{clientPort, serverPort}
	defer func() { defaultPorts[ChannelCTRL] = origPorts }()

	// Also override PKT to avoid port conflicts.
	pktClient, pktServer := pickFreePorts(t)
	origPKT := defaultPorts[ChannelPKT]
	defaultPorts[ChannelPKT] = [2]int{pktClient, pktServer}
	defer func() { defaultPorts[ChannelPKT] = origPKT }()

	mock := newMockServer(t, serverPort)
	defer mock.close()

	// PKT mock (just to allow Connect to succeed).
	pktMock := newMockServer(t, pktServer)
	defer pktMock.close()

	c := New(WithReadTimeout(2 * time.Second))
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	// Simulate GET_INFO response: [0x02, 0x10, 0x00, 0x01, 0xFF, 0x00, 0x00, 0x03]
	reply := []byte{0x02, 0x10, 0x00, 0x01, 0xFF, 0x00, 0x00, 0x03}
	go mock.respond(reply)

	info, err := c.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.NetworkID != 0x0010 {
		t.Errorf("NetworkID = 0x%04X, want 0x0010", info.NetworkID)
	}
	if info.Address != 0x01 {
		t.Errorf("Address = %d, want 1", info.Address)
	}
	if info.State != 0x03 {
		t.Errorf("State = %d, want 3", info.State)
	}
}

func TestSendCTRL_Timeout(t *testing.T) {
	clientPort, serverPort := pickFreePorts(t)

	origPorts := defaultPorts[ChannelCTRL]
	defaultPorts[ChannelCTRL] = [2]int{clientPort, serverPort}
	defer func() { defaultPorts[ChannelCTRL] = origPorts }()

	pktClient, pktServer := pickFreePorts(t)
	origPKT := defaultPorts[ChannelPKT]
	defaultPorts[ChannelPKT] = [2]int{pktClient, pktServer}
	defer func() { defaultPorts[ChannelPKT] = origPKT }()

	// Server listens but never responds.
	mock := newMockServer(t, serverPort)
	defer mock.close()
	pktMock := newMockServer(t, pktServer)
	defer pktMock.close()

	c := New(WithReadTimeout(100 * time.Millisecond))
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Close()

	_, err := c.SendCTRL(ctx, []byte{OpGetInfo})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestPKTEvents(t *testing.T) {
	clientPort, serverPort := pickFreePorts(t)
	origPorts := defaultPorts[ChannelCTRL]
	defaultPorts[ChannelCTRL] = [2]int{clientPort, serverPort}
	defer func() { defaultPorts[ChannelCTRL] = origPorts }()

	pktClient, pktServer := pickFreePorts(t)
	origPKT := defaultPorts[ChannelPKT]
	defaultPorts[ChannelPKT] = [2]int{pktClient, pktServer}
	defer func() { defaultPorts[ChannelPKT] = origPKT }()

	ctrlMock := newMockServer(t, serverPort)
	defer ctrlMock.close()
	pktMock := newMockServer(t, pktServer)
	defer pktMock.close()

	c := New(WithReadTimeout(2 * time.Second))
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Send a PKT event from the mock server to the client.
	payload := []byte{0xAA, 0xBB, 0xCC}
	go func() {
		// Give the recv loop time to start.
		time.Sleep(50 * time.Millisecond)
		clientAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: pktClient}
		pktMock.conn.WriteToUDP(payload, clientAddr)
	}()

	select {
	case ev := <-c.Events():
		if ev.Channel != ChannelPKT {
			t.Errorf("Channel = %d, want %d", ev.Channel, ChannelPKT)
		}
		if len(ev.Data) != 3 || ev.Data[0] != 0xAA {
			t.Errorf("Data = %x, want aabbcc", ev.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PKT event")
	}

	c.Close()
}
