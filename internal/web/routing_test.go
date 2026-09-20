package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/config"
)

// stubCamera satisfait camera.Client sans aucune I/O MCU. Les tests de
// routage ne pilotent pas de matériel : ils vérifient qu'une URL atteint le
// bon handler, pas ce que le handler fait du MCU. Conformément à la
// convention du projet, rien ici ne simule le comportement radio.
type stubCamera struct {
	sensors []camera.Sensor
}

func (c *stubCamera) Connect(context.Context) error { return nil }
func (c *stubCamera) Sensors(context.Context) ([]camera.Sensor, error) {
	return c.sensors, nil
}
func (c *stubCamera) CachedSensors() []camera.Sensor { return c.sensors }
func (c *stubCamera) ReadSensor(context.Context, int, string, []string) (*camera.Sensor, error) {
	return nil, nil
}
func (c *stubCamera) StartPairing(context.Context, string, string) (int, error) { return 1, nil }
func (c *stubCamera) PollPairing(context.Context, int) (*camera.Sensor, bool, error) {
	return nil, false, nil
}
func (c *stubCamera) StopPairing(context.Context, int) error  { return nil }
func (c *stubCamera) DeleteSensor(context.Context, int) error { return nil }
func (c *stubCamera) EndpointsRead(context.Context, int, []string) ([]camera.EndpointValue, error) {
	return nil, nil
}
func (c *stubCamera) EndpointsWrite(context.Context, int, []camera.EndpointWriteEntry) error {
	return nil
}
func (c *stubCamera) OpenStream(context.Context) (camera.StreamInfo, error) {
	return camera.StreamInfo{Port: 9000, Passphrase: "x"}, nil
}
func (c *stubCamera) SendPKT(context.Context, []byte) error   { return nil }
func (c *stubCamera) TriggerSiren(context.Context, int) error { return nil }
func (c *stubCamera) TriggerSirenAlarm(context.Context, int, time.Duration) error {
	return nil
}
func (c *stubCamera) StopSiren(context.Context, int) error   { return nil }
func (c *stubCamera) SetShutter(context.Context, bool) error { return nil }
func (c *stubCamera) Events() <-chan camera.SensorEvent      { return nil }
func (c *stubCamera) Close() error                           { return nil }

// newTestServer monte un Server complet avec un mux réel, pour pouvoir
// router de vraies requêtes. Le store pointe vers un fichier temporaire.
func newTestServer(t *testing.T, sensors []camera.Sensor) http.Handler {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(c *config.Config) {
		for _, s := range sensors {
			c.Sensors = append(c.Sensors, config.SensorEntry{ID: s.ID, Type: s.Type})
		}
	}); err != nil {
		t.Fatalf("store.Update: %v", err)
	}

	staticFS := fstest.MapFS{
		"static/index.html": &fstest.MapFile{Data: []byte("<html>ui</html>")},
	}
	s := NewServer(&stubCamera{sensors: sensors}, store,
		func() bool { return false }, &MQTTCallbacks{}, staticFS,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	mux, err := s.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	// basicAuth est inclus : sans mot de passe configuré c'est un no-op,
	// mais on route à travers la même chaîne qu'en production.
	return s.basicAuth(mux)
}

// do exécute une requête contre le handler et renvoie code + body décodé.
func do(t *testing.T, h http.Handler, method, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// TestUnversionedRoutesAreGone : les anciennes URLs ont été supprimées sans
// alias. Le risque n'est pas le 404 mais le faux 200 — sans catch-all, un
// GET /api/status tomberait sur le FileServer et recevrait l'index.html,
// qu'un vieux client interpréterait comme une réponse valide.
func TestUnversionedRoutesAreGone(t *testing.T) {
	h := newTestServer(t, nil)

	for _, path := range []string{
		"/api/status", "/api/sensors", "/api/config", "/api/alarm",
		"/api/codes", "/api/reboot", "/api/update/check", "/api/events",
	} {
		t.Run(path, func(t *testing.T) {
			code, body := do(t, h, "GET", path)
			if code != http.StatusGone {
				t.Errorf("GET %s = %d, want 410", path, code)
			}
			if body["error"] == nil {
				t.Errorf("GET %s : pas de champ error JSON (body=%v)", path, body)
			}
		})
	}
}

// TestV1RoutesAreServed : chaque route lue par l'UI doit atteindre un
// handler. On n'assert pas le succès métier (le stub ne pilote rien), juste
// l'absence de 404/405/410 — c'est-à-dire que l'URL existe bien.
func TestV1RoutesAreServed(t *testing.T) {
	h := newTestServer(t, []camera.Sensor{{ID: 3, Type: "KPD"}})

	routes := []struct{ method, path string }{
		{"GET", "/api/v1/status"},
		{"GET", "/api/v1/sensors"},
		{"GET", "/api/v1/alarm"},
		{"GET", "/api/v1/config"},
		{"GET", "/api/v1/config/mqtt"},
		{"GET", "/api/v1/config/homekit"},
		{"GET", "/api/v1/config/admin"},
		{"GET", "/api/v1/config/alarm"},
		{"GET", "/api/v1/config/web"},
		{"GET", "/api/v1/kpd/code"},
	}
	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			code, _ := do(t, h, r.method, r.path)
			switch code {
			case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone:
				t.Errorf("%s %s = %d : route non servie", r.method, r.path, code)
			}
		})
	}
}

// TestKPDCodeResolution couvre le bug corrigé : l'ancien findKPDSensor
// renvoyait le premier KPD du slice, donc avec deux claviers appairés
// l'écriture du code partait vers l'un ou l'autre selon l'ordre de
// persistance, sans que l'utilisateur puisse le savoir.
func TestKPDCodeResolution(t *testing.T) {
	cases := []struct {
		name    string
		sensors []camera.Sensor
		want    int
	}{
		{"aucun clavier", []camera.Sensor{{ID: 1, Type: "DWS"}}, http.StatusNotFound},
		{"un seul clavier", []camera.Sensor{{ID: 3, Type: "KPD"}}, http.StatusOK},
		{
			"deux claviers = ambigu",
			[]camera.Sensor{{ID: 3, Type: "KPD"}, {ID: 4, Type: "KPD"}},
			http.StatusConflict,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestServer(t, tc.sensors)
			code, _ := do(t, h, "GET", "/api/v1/kpd/code")
			if code != tc.want {
				t.Errorf("GET /api/v1/kpd/code = %d, want %d", code, tc.want)
			}
		})
	}
}

// TestUnknownV1RouteIs404 : une route /api/v1 inconnue doit répondre 404, pas
// 410. Confondre les deux ferait répondre « utiliser /api/v1 » à un client
// déjà sur /api/v1, et 410 étant terminal, un cache pourrait condamner
// définitivement une route ajoutée dans une version ultérieure.
func TestUnknownV1RouteIs404(t *testing.T) {
	h := newTestServer(t, nil)

	cases := []struct{ method, path string }{
		{"GET", "/api/v1/typo"},
		{"GET", "/api/v1/update/check"}, // ancienne forme, sous v1
		{"POST", "/api/v1/sensors"},     // méthode non supportée
		{"PATCH", "/api/v1/config/mqtt"},
		{"POST", "/api/v1/commands/unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			code, body := do(t, h, tc.method, tc.path)
			if code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", tc.method, tc.path, code)
			}
			if body["error"] == nil {
				t.Errorf("%s %s : pas de champ error JSON", tc.method, tc.path)
			}
		})
	}
}

// TestPairLiteralDoesNotHitSensorID : le segment littéral `pair` partage son
// niveau avec {id}. Une requête sans session ne doit pas être présentée comme
// un ID de capteur invalide.
func TestPairLiteralDoesNotHitSensorID(t *testing.T) {
	h := newTestServer(t, nil)
	for _, m := range []string{"DELETE", "PUT"} {
		code, body := do(t, h, m, "/api/v1/sensors/pair")
		if code != http.StatusNotFound {
			t.Errorf("%s /api/v1/sensors/pair = %d, want 404", m, code)
		}
		if msg, _ := body["error"].(string); strings.Contains(msg, "ID capteur") {
			t.Errorf("%s /api/v1/sensors/pair : message trompeur %q", m, msg)
		}
	}
}
