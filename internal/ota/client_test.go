package ota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMockGitHub renvoie un httptest.Server qui répond avec le body donné
// sur /repos/<repo>/releases/latest, et un client OTA configuré pour le
// taper.
func newMockGitHub(t *testing.T, status int, body string) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/releases") {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	c := NewClient("v0.1.0", nil)
	c.http = srv.Client()
	// Détourner les appels GitHub vers le mock — on remplace l'URL en
	// dur via le champ repo + une indirection sur l'URL de base.
	c.repo = "mock/mock"
	// Override interne : remplacer le builder d'URL n'étant pas exposé,
	// on patch l'HTTP client pour rediriger api.github.com → srv.URL.
	c.http = &http.Client{
		Transport: rewriteTransport{
			base:   srv.URL,
			target: "https://api.github.com",
			inner:  http.DefaultTransport,
		},
		Timeout: 5 * time.Second,
	}
	return srv, c
}

// rewriteTransport redirige les requêtes vers `target` (ex https://api.github.com)
// vers `base` (ex http://127.0.0.1:54321).
type rewriteTransport struct {
	base   string
	target string
	inner  http.RoundTripper
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasPrefix(req.URL.String(), t.target) {
		newURL := t.base + strings.TrimPrefix(req.URL.String(), t.target)
		nr, err := http.NewRequestWithContext(req.Context(), req.Method, newURL, req.Body)
		if err != nil {
			return nil, err
		}
		nr.Header = req.Header
		return t.inner.RoundTrip(nr)
	}
	return t.inner.RoundTrip(req)
}

func TestCheckLatest_UpdateAvailable(t *testing.T) {
	body := `[{"tag_name":"v0.2.0","name":"Release 0.2.0","body":"Notes","html_url":"https://example/r/v0.2.0","prerelease":false}]`
	srv, c := newMockGitHub(t, 200, body)
	defer srv.Close()

	res, err := c.CheckLatest(context.Background(), false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if !res.UpdateNeeded {
		t.Error("expected UpdateNeeded=true (v0.1.0 → v0.2.0)")
	}
	if res.Latest.TagName != "v0.2.0" {
		t.Errorf("Latest.TagName = %q, want v0.2.0", res.Latest.TagName)
	}
	if res.Current != "v0.1.0" {
		t.Errorf("Current = %q, want v0.1.0", res.Current)
	}
}

func TestCheckLatest_UpToDate(t *testing.T) {
	body := `[{"tag_name":"v0.1.0","name":"r","body":"","html_url":"","prerelease":false}]`
	srv, c := newMockGitHub(t, 200, body)
	defer srv.Close()

	res, err := c.CheckLatest(context.Background(), false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.UpdateNeeded {
		t.Error("expected UpdateNeeded=false when current == latest")
	}
}

func TestCheckLatest_NoReleasesYet(t *testing.T) {
	// Liste vide = aucune release publiée encore.
	srv, c := newMockGitHub(t, 200, "[]")
	defer srv.Close()

	res, err := c.CheckLatest(context.Background(), false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.UpdateNeeded {
		t.Error("empty release list = no update")
	}
}

func TestCheckLatest_DevBuild(t *testing.T) {
	body := `[{"tag_name":"v0.5.0","name":"r","body":"","html_url":"","prerelease":false}]`
	srv, c := newMockGitHub(t, 200, body)
	defer srv.Close()
	c.currentVer = "dev"

	res, err := c.CheckLatest(context.Background(), false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.UpdateNeeded {
		t.Error("dev builds should never be offered an update")
	}
}

func TestCheckLatest_BuildInfoSuffix(t *testing.T) {
	// BuildInfo() peut renvoyer "v0.1.0 (abc1234, 2026-05-15)".
	// shouldOfferUpdate doit extraire juste "v0.1.0" pour la comparaison.
	body := `[{"tag_name":"v0.1.0","name":"r","body":"","html_url":"","prerelease":false}]`
	srv, c := newMockGitHub(t, 200, body)
	defer srv.Close()
	c.currentVer = "v0.1.0 (abc1234, 2026-05-15)"

	res, err := c.CheckLatest(context.Background(), false)
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if res.UpdateNeeded {
		t.Errorf("UpdateNeeded should be false when current tag matches latest, got %+v", res)
	}
}

func TestCheckLatest_CachesResult(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"tag_name":"v0.2.0","html_url":"","prerelease":false}]`))
	}))
	defer srv.Close()
	c := NewClient("v0.1.0", nil)
	c.http = &http.Client{
		Transport: rewriteTransport{base: srv.URL, target: "https://api.github.com", inner: http.DefaultTransport},
	}

	_, _ = c.CheckLatest(context.Background(), false)
	_, _ = c.CheckLatest(context.Background(), false)
	_, _ = c.CheckLatest(context.Background(), false)
	if calls != 1 {
		t.Errorf("expected 1 GitHub call (rest from cache), got %d", calls)
	}

	// force=true contourne le cache.
	_, _ = c.CheckLatest(context.Background(), true)
	if calls != 2 {
		t.Errorf("expected force=true to refetch, got %d calls", calls)
	}
}
