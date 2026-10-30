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

// TestUpdateAlarm_ShutterFollowsAlarm : l'option se sauvegarde et se relit
// par l'API, et un PUT qui ne la mentionne pas la laisse telle quelle.
func TestUpdateAlarm_ShutterFollowsAlarm(t *testing.T) {
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

	put := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/config/alarm", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT %s = %d : %s", body, rec.Code, rec.Body)
		}
	}
	get := func() bool {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/config/alarm", nil))
		var got struct {
			ShutterFollowsAlarm bool `json:"shutter_follows_alarm"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("GET : %v", err)
		}
		return got.ShutterFollowsAlarm
	}

	if get() {
		t.Fatal("option active par défaut")
	}
	put(`{"shutter_follows_alarm":true}`)
	if !store.Get().Alarm.ShutterFollowsAlarm || !get() {
		t.Fatal("option non sauvegardée")
	}
	put(`{"wail_duration_seconds":120}`)
	if !store.Get().Alarm.ShutterFollowsAlarm {
		t.Fatal("un PUT sans l'option l'a désactivée")
	}
	put(`{"shutter_follows_alarm":false}`)
	if get() {
		t.Fatal("option non désactivée")
	}
}
