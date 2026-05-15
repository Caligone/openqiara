package web

import (
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackRequest(t *testing.T) {
	cases := []struct {
		remote string
		want   bool
	}{
		{"127.0.0.1:54321", true},
		{"[::1]:54321", true},
		{"10.0.0.1:80", false},
		{"192.168.1.5:1234", false},
		{"[fe80::1]:80", false},
		// Defensive: RemoteAddr could in theory be a bare hostname.
		{"not-an-ip", false},
		{"", false},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/events", nil)
		r.RemoteAddr = tc.remote
		got := isLoopbackRequest(r)
		if got != tc.want {
			t.Errorf("isLoopbackRequest(%q) = %v, want %v", tc.remote, got, tc.want)
		}
	}
}
