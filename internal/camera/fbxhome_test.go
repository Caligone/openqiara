package camera

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockRunner simulates fbxbusctl for tests.
type mockRunner struct {
	output []byte
	err    error
}

func (m mockRunner) Run(_ context.Context, _ string, _ ...string) ([]byte, error) {
	return m.output, m.err
}

func newTestServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	return httptest.NewServer(mux)
}

func newTestClient(t *testing.T, url string) *FbxhomeClient {
	t.Helper()
	return NewFbxhomeClient(
		WithPrivURL(url),
		WithHomeURL(url),
		WithCommandRunner(mockRunner{output: []byte("test-session-42\n")}),
	)
}

func TestConnect(t *testing.T) {
	c := NewFbxhomeClient(
		WithCommandRunner(mockRunner{output: []byte("session-abc\n")}),
	)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if c.sessionID != "session-abc" {
		t.Errorf("sessionID = %q, want %q", c.sessionID, "session-abc")
	}
}

func TestConnect_EmptyToken(t *testing.T) {
	c := NewFbxhomeClient(
		WithCommandRunner(mockRunner{output: []byte("  \n")}),
	)
	if err := c.Connect(context.Background()); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSensors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc/get_domus_nodes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		resp := domusNodesResponse{
			Result: []domusNode{
				{
					ID:       33,
					TypeName: "Node.DomusNode.HlDws",
					ItemID:   "562e741c4f5bd5e6",
					Values: domusValues{
						Reachable:   1,
						Battery:     85,
						Temperature: temperatureVal{Value: 210, Timestamp: 1700000000},
					},
				},
				{
					ID:       34,
					TypeName: "Node.DomusNode.HlPir",
					ItemID:   "abcd1234",
					Values:   domusValues{Reachable: 0},
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	sensors, err := c.Sensors(context.Background())
	if err != nil {
		t.Fatalf("Sensors: %v", err)
	}
	if len(sensors) != 2 {
		t.Fatalf("got %d sensors, want 2", len(sensors))
	}

	s := sensors[0]
	if s.ID != 33 {
		t.Errorf("ID = %d, want 33", s.ID)
	}
	if s.Type != "DWS" {
		t.Errorf("Type = %q, want DWS", s.Type)
	}
	if !s.Reachable {
		t.Error("expected reachable")
	}
	if s.Battery != 85 {
		t.Errorf("Battery = %d, want 85", s.Battery)
	}
	if s.Temperature != 210 {
		t.Errorf("Temperature = %d, want 210", s.Temperature)
	}

	if sensors[1].Type != "PIR" {
		t.Errorf("sensor[1].Type = %q, want PIR", sensors[1].Type)
	}
	if sensors[1].Reachable {
		t.Error("sensor[1] should not be reachable")
	}
}

func TestReadSensor(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/home/endpoints_read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.Header.Get("X-Hlcore-Session-Id") != "test-session-42" {
			t.Errorf("missing or wrong session header: %q", r.Header.Get("X-Hlcore-Session-Id"))
		}

		var req endpointsReadRequest
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.List) == 0 || req.List[0].NodeID != 33 {
			t.Error("unexpected request body")
		}

		resp := endpointsReadResponse{
			List: []endpointResult{{
				NodeID: 33,
				EPValues: []endpointValue{
					{EPName: "state", Value: true},
					{EPName: "temperature", Value: map[string]interface{}{"value": float64(200), "timestamp": float64(1700000000)}},
					{EPName: "battery", Value: float64(90)},
				},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	s, err := c.ReadSensor(context.Background(), 33, []string{"state", "temperature", "battery"})
	if err != nil {
		t.Fatalf("ReadSensor: %v", err)
	}
	if !s.Open {
		t.Error("expected Open=true")
	}
	if s.Battery != 90 {
		t.Errorf("Battery = %d, want 90", s.Battery)
	}
	if s.Temperature != 200 {
		t.Errorf("Temperature = %d, want 200", s.Temperature)
	}
}

func TestReadSensor_ReauthOn401(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/home/endpoints_read", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		resp := endpointsReadResponse{
			List: []endpointResult{{
				NodeID:   33,
				EPValues: []endpointValue{{EPName: "state", Value: false}},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	s, err := c.ReadSensor(context.Background(), 33, []string{"state"})
	if err != nil {
		t.Fatalf("ReadSensor after reauth: %v", err)
	}
	if s.Open {
		t.Error("expected Open=false after reauth")
	}
	if calls != 2 {
		t.Errorf("expected 2 calls, got %d", calls)
	}
}

func TestStartPairing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/home/pairing", func(w http.ResponseWriter, r *http.Request) {
		var req pairingRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Op != "start_adapter" {
			t.Errorf("op = %q, want start_adapter", req.Op)
		}
		if req.NodeType != "HOMELABDWS" {
			t.Errorf("node_type = %q, want HOMELABDWS", req.NodeType)
		}

		resp := pairingResponse{Session: 2127910, LayoutName: "WaitItem", Refresh: 10000}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	session, err := c.StartPairing(context.Background(), "DWS", "")
	if err != nil {
		t.Fatalf("StartPairing: %v", err)
	}
	if session != 2127910 {
		t.Errorf("session = %d, want 2127910", session)
	}
}

func TestStartPairing_UnknownType(t *testing.T) {
	c := NewFbxhomeClient()
	_, err := c.StartPairing(context.Background(), "UNKNOWN", "")
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestPollPairing_Terminated(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/home/pairing", func(w http.ResponseWriter, r *http.Request) {
		resp := pairingResponse{
			Last:       1,
			LayoutName: "Terminated",
			NodeID:     33,
			NodeType:   "Node.DomusNode.HlDws",
			Session:    2127910,
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	sensor, done, err := c.PollPairing(context.Background(), 2127910)
	if err != nil {
		t.Fatalf("PollPairing: %v", err)
	}
	if !done {
		t.Error("expected done=true")
	}
	if sensor.ID != 33 {
		t.Errorf("sensor.ID = %d, want 33", sensor.ID)
	}
	if sensor.Type != "DWS" {
		t.Errorf("sensor.Type = %q, want DWS", sensor.Type)
	}
}

func TestPollPairing_Waiting(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/home/pairing", func(w http.ResponseWriter, r *http.Request) {
		resp := pairingResponse{LayoutName: "WaitItem", Session: 2127910, Refresh: 10000}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	sensor, done, err := c.PollPairing(context.Background(), 2127910)
	if err != nil {
		t.Fatalf("PollPairing: %v", err)
	}
	if done {
		t.Error("expected done=false")
	}
	if sensor != nil {
		t.Error("expected nil sensor while waiting")
	}
}

func TestStopPairing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/home/pairing", func(w http.ResponseWriter, r *http.Request) {
		var req pairingRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Op != "stop" {
			t.Errorf("op = %q, want stop", req.Op)
		}
		if req.Session != 123 {
			t.Errorf("session = %d, want 123", req.Session)
		}
		json.NewEncoder(w).Encode(pairingResponse{})
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	if err := c.StopPairing(context.Background(), 123); err != nil {
		t.Fatalf("StopPairing: %v", err)
	}
}

func TestEvents_PollEmitsOnChange(t *testing.T) {
	callCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/rpc/get_domus_nodes", func(w http.ResponseWriter, r *http.Request) {
		resp := domusNodesResponse{
			Result: []domusNode{{
				ID: 33, TypeName: "Node.DomusNode.HlDws", ItemID: "abc",
				Values: domusValues{Reachable: 1},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/api/v1/home/endpoints_read", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// State changes on second call.
		state := false
		if callCount >= 2 {
			state = true
		}
		resp := endpointsReadResponse{
			List: []endpointResult{{
				NodeID:   33,
				EPValues: []endpointValue{{EPName: "state", Value: state}},
			}},
		}
		json.NewEncoder(w).Encode(resp)
	})

	srv := newTestServer(t, mux)
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.Connect(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	c.StartPolling(ctx, 50*time.Millisecond)

	// First poll: new sensor, should emit event.
	select {
	case ev := <-c.Events():
		if ev.SensorID != 33 {
			t.Errorf("event sensor ID = %d, want 33", ev.SensorID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for first event")
	}

	// Second poll: state changes, should emit another event.
	select {
	case ev := <-c.Events():
		if !ev.Sensor.Open {
			t.Error("expected Open=true on state change event")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for state change event")
	}

	c.Close()
}

func TestClose_Idempotent(t *testing.T) {
	c := NewFbxhomeClient()
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
