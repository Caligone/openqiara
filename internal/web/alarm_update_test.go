package web

import (
	"encoding/json"
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

// TestUpdateAlarm_ShutterFollows : l'option se sauvegarde et se relit par
// l'API, une valeur inconnue est refusée, et un PUT qui ne la mentionne pas
// la laisse telle quelle.
func TestUpdateAlarm_ShutterFollows(t *testing.T) {
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	s := NewServer(&stubCamera{}, store,
		func() bool { return false }, &MQTTCallbacks{},
		fstest.MapFS{"static/index.html": &fstest.MapFile{Data: []byte("ui")}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux, err := s.routes()
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	h := s.basicAuth(mux)

	put := func(body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/config/alarm", strings.NewReader(body)))
		return rec.Code
	}
	get := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config/alarm", nil))
		var got struct {
			ShutterFollows string `json:"shutter_follows"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("GET : %v", err)
		}
		return got.ShutterFollows
	}

	if got := get(); got != "none" {
		t.Fatalf("par défaut : %q, attendu none", got)
	}
	for _, v := range []string{"away", "all"} {
		if code := put(`{"shutter_follows":"` + v + `"}`); code != http.StatusOK {
			t.Fatalf("PUT %s = %d", v, code)
		}
		if store.Get().Alarm.ShutterFollows != v || get() != v {
			t.Fatalf("%s non sauvegardé", v)
		}
	}
	if code := put(`{"wail_duration_seconds":120}`); code != http.StatusOK || get() != "all" {
		t.Fatalf("un PUT sans l'option l'a changée (%d, %q)", code, get())
	}
	if code := put(`{"shutter_follows":"night"}`); code != http.StatusBadRequest || get() != "all" {
		t.Fatalf("valeur inconnue : %d, option %q", code, get())
	}
	if code := put(`{"shutter_follows":"none"}`); code != http.StatusOK || get() != "none" {
		t.Fatalf("option non désactivée (%d, %q)", code, get())
	}
}
