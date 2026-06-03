package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthorized(t *testing.T) {
	cases := []struct {
		name       string
		token      string
		remoteAddr string
		header     string
		want       bool
	}{
		{"disabled allows non-loopback", "", "192.0.2.5:1234", "", true},
		{"enabled loopback open no header", "secret", "127.0.0.1:5050", "", true},
		{"enabled loopback ipv6 open", "secret", "[::1]:5050", "", true},
		{"enabled remote no header rejected", "secret", "192.0.2.5:1234", "", false},
		{"enabled remote bad token rejected", "secret", "192.0.2.5:1234", "Bearer nope", false},
		{"enabled remote good token allowed", "secret", "192.0.2.5:1234", "Bearer secret", true},
		{"enabled remote non-bearer rejected", "secret", "192.0.2.5:1234", "secret", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &HTTPServer{authToken: c.token}
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			req.RemoteAddr = c.remoteAddr
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			if got := h.authorized(req); got != c.want {
				t.Errorf("authorized = %v, want %v", got, c.want)
			}
		})
	}
}

func TestWithAuthRejectsWith401(t *testing.T) {
	d, cleanup := newCreateSessionTestDaemon(t)
	defer cleanup()
	h := &HTTPServer{daemon: d, authToken: "secret"}
	wrapped := h.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Non-loopback, no token -> 401, inner handler never runs.
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.RemoteAddr = "192.0.2.9:4321"
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
