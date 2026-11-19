package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A request body past maxBodyBytes is refused with a 400.
func TestOversizedBodyRefused(t *testing.T) {
	h := newTestServer(t, nil)
	big := `{"label":"` + strings.Repeat("a", maxBodyBytes+1024) + `"}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/sensors/3", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d for an oversized body", rec.Code)
	}
	// A normal body still goes through.
	req = httptest.NewRequest(http.MethodPut, "/api/v1/sensors/3", strings.NewReader(`{"label":"Porte"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d for a small body: %s", rec.Code, rec.Body.String())
	}
}
