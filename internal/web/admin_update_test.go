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

const testAdminPassword = "motdepasse"

// adminTestServer renvoie un handler dont l'auth admin est déjà activée.
func adminTestServer(t *testing.T) (http.Handler, *config.Store) {
	t.Helper()
	store := config.NewStore(filepath.Join(t.TempDir(), "config.json"))
	hash, err := config.HashAdminPassword(testAdminPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := store.Update(func(c *config.Config) { c.Admin.PasswordHash = hash }); err != nil {
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

func adminRequest(t *testing.T, h http.Handler, method, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/api/v1/config/admin", strings.NewReader(body))
	req.SetBasicAuth("admin", testAdminPassword)
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestUpdateAdmin_EmptyPasswordRejected : #45 — un champ vide ne doit jamais
// désactiver l'auth. Seul DELETE le peut.
func TestUpdateAdmin_EmptyPasswordRejected(t *testing.T) {
	h, store := adminTestServer(t)

	for _, body := range []string{`{"password":""}`, `{}`} {
		if code := adminRequest(t, h, http.MethodPut, body); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, code)
		}
	}
	if !store.Get().Admin.AuthEnabled() {
		t.Fatal("auth désactivée par un PUT vide")
	}
}

func TestDeleteAdmin_DisablesAuth(t *testing.T) {
	h, store := adminTestServer(t)

	if code := adminRequest(t, h, http.MethodDelete, ""); code != http.StatusOK {
		t.Fatalf("DELETE = %d, want 200", code)
	}
	if store.Get().Admin.AuthEnabled() {
		t.Fatal("auth toujours active après DELETE")
	}
}

func TestUpdateAdmin_ChangesPassword(t *testing.T) {
	h, store := adminTestServer(t)

	if code := adminRequest(t, h, http.MethodPut, `{"password":"nouveaumdp"}`); code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", code)
	}
	admin := store.Get().Admin
	if !admin.CheckPassword("nouveaumdp") || admin.CheckPassword(testAdminPassword) {
		t.Fatal("mot de passe non remplacé")
	}
}
