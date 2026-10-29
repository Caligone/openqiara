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

// TestKPDCodeLegacyRoutes: /kpd/code, the API from when the keypad had a
// single code, reads, replaces and deletes the first code and leaves the
// others alone.
func TestKPDCodeLegacyRoutes(t *testing.T) {
	h, store := kpdTestServer(t, []config.KPDCode{{Password: "1111", Label: "Moi"}, {Password: "2222", Label: "Invité"}})

	if code, body := kpdRequest(t, h, "GET", "/api/v1/kpd/code", ""); code != http.StatusOK ||
		!strings.Contains(body, `"code":{"label":"Moi","password":"1111"}`) {
		t.Errorf("GET = %d %s, want the first code", code, body)
	}
	if code, body := kpdRequest(t, h, "PUT", "/api/v1/kpd/code", `{"password":"3333"}`); code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	want := []config.KPDCode{{Password: "3333", Label: "Code"}, {Password: "2222", Label: "Invité"}}
	if got := storedCodes(store); !reflect.DeepEqual(got, want) {
		t.Errorf("after PUT: %+v, want %+v", got, want)
	}
	if code, body := kpdRequest(t, h, "PUT", "/api/v1/kpd/code", `{"password":"2222"}`); code != http.StatusBadRequest {
		t.Errorf("PUT of the second code as the first = %d %s, want 400", code, body)
	}
	if code, body := kpdRequest(t, h, "DELETE", "/api/v1/kpd/code", ""); code != http.StatusOK {
		t.Fatalf("DELETE = %d %s", code, body)
	}
	if got := storedCodes(store); !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("after DELETE: %+v, want %+v", got, want[1:])
	}
}
