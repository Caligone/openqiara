package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/caligone/openqiara/internal/config"
)

// mqttTestServer builds a Server whose store is seeded with a TLS-configured
// broker, and returns both the auth-wrapped handler and the store so a test can
// PUT and then read back the persisted config.
func mqttTestServer(t *testing.T) (http.Handler, *config.Store) {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(c *config.Config) {
		c.MQTT = config.MQTTConfig{
			Broker:      "ssl://broker:8883",
			Username:    "openqiara",
			TopicPrefix: "openqiara",
			TLSCACert:   "/data/mqtt/ca.pem",
		}
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	s := NewServer(&stubCamera{}, store,
		func() bool { return false }, &MQTTCallbacks{},
		fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ui")}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux, err := s.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	return s.basicAuth(mux), store
}

func putMQTT(t *testing.T, h http.Handler, jsonBody string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/mqtt", strings.NewReader(jsonBody))
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestUpdateMQTT_NeverTouchesTLS ensures the web PUT handler cannot alter the
// file-only TLS configuration. TLS is set out-of-band (openqiara.json + cert
// files on disk); the handler ignores any tls_* key in the body, so it can
// neither wipe a working setup nor repoint certificates via the web API — even
// when a client explicitly sends tls_* values.
func TestUpdateMQTT_NeverTouchesTLS(t *testing.T) {
	h, store := mqttTestServer(t)

	// A body that both omits and explicitly sets tls_* must leave TLS intact.
	body := `{"broker":"ssl://broker:8883","topic_prefix":"newprefix",` +
		`"tls_ca_cert":"/evil/ca.pem","tls_client_cert":"/evil/c.pem",` +
		`"tls_client_key":"/evil/k.pem","tls_insecure":true}`
	if code := putMQTT(t, h, body); code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", code)
	}

	got := store.Get().MQTT
	if got.TLSCACert != "/data/mqtt/ca.pem" {
		t.Errorf("PUT altered TLSCACert: got %q, want /data/mqtt/ca.pem", got.TLSCACert)
	}
	if got.TLSClientCert != "" || got.TLSClientKey != "" {
		t.Errorf("PUT set client cert/key: got %q / %q, want empty", got.TLSClientCert, got.TLSClientKey)
	}
	if got.TLSInsecure {
		t.Error("PUT set TLSInsecure to true; want unchanged (false)")
	}
	if got.TopicPrefix != "newprefix" {
		t.Errorf("TopicPrefix not applied: got %q, want newprefix", got.TopicPrefix)
	}
}

// TestUpdateMQTT_RejectsPlaintextBrokerWithTLS ensures the web UI cannot switch
// a file-configured TLS setup to a plaintext broker, which would only surface
// as an MQTT start failure at the next boot.
func TestUpdateMQTT_RejectsPlaintextBrokerWithTLS(t *testing.T) {
	h, store := mqttTestServer(t)

	if code := putMQTT(t, h, `{"broker":"tcp://broker:1883"}`); code != http.StatusBadRequest {
		t.Fatalf("PUT status = %d, want 400", code)
	}
	if got := store.Get().MQTT.Broker; got != "ssl://broker:8883" {
		t.Errorf("broker changed to %q, want ssl://broker:8883", got)
	}
}
