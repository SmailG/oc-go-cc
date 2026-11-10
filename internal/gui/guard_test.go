package gui

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsLoopbackHostPort(t *testing.T) {
	tests := []struct {
		name     string
		hostport string
		want     bool
	}{
		{"ipv4 loopback", "127.0.0.1:3445", true},
		{"localhost", "localhost:3445", true},
		{"localhost case", "LocalHost:3445", true},
		{"ipv6 loopback", "[::1]:3445", true},
		{"other loopback address", "127.0.0.2:3445", true},
		{"rebound domain", "evil.example:3445", false},
		{"wrong port", "127.0.0.1:9999", false},
		{"missing port", "127.0.0.1", false},
		{"wildcard", "0.0.0.0:3445", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLoopbackHostPort(tc.hostport, 3445); got != tc.want {
				t.Errorf("isLoopbackHostPort(%q) = %v, want %v", tc.hostport, got, tc.want)
			}
		})
	}
}

// A rebinding page reaches the loopback address but keeps its own hostname in
// Host, so the guard must refuse it before routing.
func TestRequestGuard_RejectsForeignHost(t *testing.T) {
	var reached bool
	h := requestGuardMiddleware(3445, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	req := httptest.NewRequest(http.MethodGet, "http://evil.example:3445/api/proxy/config", nil)
	req.Host = "evil.example:3445"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if reached {
		t.Error("a foreign host reached the handler")
	}
}

func TestRequestGuard_OriginRules(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		origin string
		want   int
	}{
		{"read without origin", http.MethodGet, "/api/proxy/config", "", http.StatusOK},
		{"static read", http.MethodGet, "/index.html", "", http.StatusOK},
		{"write without origin", http.MethodPost, "/api/proxy/config", "", http.StatusForbidden},
		{"write from another site", http.MethodPost, "/api/proxy/config", "https://evil.example", http.StatusForbidden},
		{"write from another loopback port", http.MethodPost, "/api/config/open", "http://127.0.0.1:9999", http.StatusForbidden},
		{"write from the dashboard", http.MethodPost, "/api/proxy/config", "http://127.0.0.1:3445", http.StatusOK},
		{"write via localhost", http.MethodPost, "/api/config/open", "http://localhost:3445", http.StatusOK},
		{"delete from the dashboard", http.MethodDelete, "/api/x", "http://[::1]:3445", http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := requestGuardMiddleware(3445, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tc.method, "http://127.0.0.1:3445"+tc.path, nil)
			req.Host = "127.0.0.1:3445"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}
