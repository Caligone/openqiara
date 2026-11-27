package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/caligone/openqiara/internal/config"
)

// kpdTestServer builds a Server whose store holds one keypad with the
// given codes, and returns the auth-wrapped handler and the store.
func kpdTestServer(t *testing.T, codes []config.KPDCode) (http.Handler, *config.Store) {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Update(func(c *config.Config) {
		c.Sensors = []config.SensorEntry{{ID: 14, Type: "KPD", KPDCodes: codes}, {ID: 23, Type: "DWS"}}
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

func kpdRequest(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec.Code, rec.Body.String()
}

func storedCodes(store *config.Store) []config.KPDCode {
	return store.Get().Sensors[0].KPDCodes
}

// TestKPDCodesRoundTrip: the list PUT is the list stored and read back,
// labels trimmed; a keypad without codes reads as an empty list, not null.
func TestKPDCodesRoundTrip(t *testing.T) {
	h, store := kpdTestServer(t, nil)

	if code, body := kpdRequest(t, h, "GET", "/api/v1/kpd/codes", ""); code != http.StatusOK || !strings.Contains(body, `"codes":[]`) {
		t.Errorf("GET without codes = %d %s, want an empty list", code, body)
	}
	code, body := kpdRequest(t, h, "PUT", "/api/v1/kpd/codes",
		`{"codes":[{"password":"1234","label":" Moi "},{"password":"5678"}]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	want := []config.KPDCode{{Password: "1234", Label: "Moi"}, {Password: "5678"}}
	if got := storedCodes(store); !reflect.DeepEqual(got, want) {
		t.Errorf("stored %+v, want %+v", got, want)
	}
	if code, body := kpdRequest(t, h, "GET", "/api/v1/kpd/codes", ""); code != http.StatusOK ||
		!strings.Contains(body, `"codes":[{"password":"1234","label":"Moi"},{"password":"5678"}]`) {
		t.Errorf("GET = %d %s", code, body)
	}
}

// TestKPDCodesRejected: a list the keypad could not use is refused whole,
// and the stored codes stay as they were.
func TestKPDCodesRejected(t *testing.T) {
	var distinct []string
	for i := range maxKPDCodes + 1 {
		distinct = append(distinct, fmt.Sprintf(`{"password":"%04d"}`, 1000+i))
	}
	tooMany := `{"codes":[` + strings.Join(distinct, ",") + `]}`
	for name, body := range map[string]string{
		"3 chiffres":    `{"codes":[{"password":"123"}]}`,
		"une lettre":    `{"codes":[{"password":"12a4"}]}`,
		"doublon":       `{"codes":[{"password":"1234"},{"password":"1234"}]}`,
		"trop":          tooMany,
		"JSON invalide": `{"codes":`,
	} {
		t.Run(name, func(t *testing.T) {
			before := []config.KPDCode{{Password: "0000", Label: "Moi"}}
			h, store := kpdTestServer(t, before)
			if code, resp := kpdRequest(t, h, "PUT", "/api/v1/kpd/codes", body); code != http.StatusBadRequest {
				t.Errorf("PUT = %d %s, want 400", code, resp)
			}
			if got := storedCodes(store); !reflect.DeepEqual(got, before) {
				t.Errorf("stored codes changed to %+v", got)
			}
		})
	}
}

// TestKPDCodesSeveralKeypads: with two keypads, sensor_id picks the one
// read or written, the other keeps its codes; without it, 409 rather than
// a keypad picked at random (#86).
func TestKPDCodesSeveralKeypads(t *testing.T) {
	h, store := kpdTestServer(t, []config.KPDCode{{Password: "1111"}})
	if err := store.Update(func(c *config.Config) {
		c.Sensors = append(c.Sensors, config.SensorEntry{ID: 31, Type: "KPD", KPDCodes: []config.KPDCode{{Password: "3333"}}})
	}); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	for _, m := range []string{"GET", "PUT"} {
		if code, _ := kpdRequest(t, h, m, "/api/v1/kpd/codes", `{"codes":[{"password":"9999"}]}`); code != http.StatusConflict {
			t.Errorf("%s without sensor_id = %d, want 409", m, code)
		}
	}
	if code, body := kpdRequest(t, h, "GET", "/api/v1/kpd/codes?sensor_id=31", ""); code != http.StatusOK ||
		!strings.Contains(body, `"sensor_id":31`) || !strings.Contains(body, "3333") {
		t.Errorf("GET sensor_id=31 = %d %s, want keypad 31's codes", code, body)
	}
	if code, body := kpdRequest(t, h, "PUT", "/api/v1/kpd/codes?sensor_id=31", `{"codes":[{"password":"4444"},{"password":"5555"}]}`); code != http.StatusOK {
		t.Fatalf("PUT sensor_id=31 = %d %s", code, body)
	}
	got := map[int][]config.KPDCode{}
	for _, se := range store.Get().Sensors {
		got[se.ID] = se.KPDCodes
	}
	if want := []config.KPDCode{{Password: "1111"}}; !reflect.DeepEqual(got[14], want) {
		t.Errorf("keypad 14 = %v, want %v untouched", got[14], want)
	}
	if want := []config.KPDCode{{Password: "4444"}, {Password: "5555"}}; !reflect.DeepEqual(got[31], want) {
		t.Errorf("keypad 31 = %v, want %v", got[31], want)
	}

	// Not a keypad, or no such sensor: 404, nothing written.
	for _, id := range []string{"23", "99", "x"} {
		if code, _ := kpdRequest(t, h, "PUT", "/api/v1/kpd/codes?sensor_id="+id, `{"codes":[{"password":"9999"}]}`); code != http.StatusNotFound {
			t.Errorf("PUT sensor_id=%s = %d, want 404", id, code)
		}
	}
}
