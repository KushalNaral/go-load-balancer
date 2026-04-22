package backend

import (
	"strings"
	"testing"
)

func TestNewBackend(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantErr     bool
		errContains string
		wantScheme  string
		wantHost    string
	}{
		// --- happy paths ---
		{
			name:       "simple http with port",
			input:      "http://localhost:8080",
			wantScheme: "http",
			wantHost:   "localhost:8080",
		},
		{
			name:       "https scheme",
			input:      "https://example.com",
			wantScheme: "https",
			wantHost:   "example.com",
		},
		{
			name:       "trailing slash path is allowed",
			input:      "http://example.com/",
			wantScheme: "http",
			wantHost:   "example.com",
		},
		{
			name:       "ipv4 host with port",
			input:      "http://127.0.0.1:9000",
			wantScheme: "http",
			wantHost:   "127.0.0.1:9000",
		},

		// --- sad paths (one per rejection rule) ---
		{
			name:        "empty string has no scheme",
			input:       "",
			wantErr:     true,
			errContains: "has no scheme",
		},
		{
			name:        "scheme not in allowlist",
			input:       "ftp://example.com",
			wantErr:     true,
			errContains: "not allowed",
		},
		{
			name:        "host-only string parses as opaque scheme",
			input:       "localhost:8080",
			wantErr:     true,
			errContains: "not allowed",
		},
		{
			name:        "missing host",
			input:       "http://",
			wantErr:     true,
			errContains: "has no host",
		},
		{
			name:        "non-root path rejected",
			input:       "http://example.com/api",
			wantErr:     true,
			errContains: "must not have a path",
		},
		{
			name:        "query string rejected",
			input:       "http://example.com?x=1",
			wantErr:     true,
			errContains: "query string",
		},
		{
			name:        "fragment rejected",
			input:       "http://example.com#frag",
			wantErr:     true,
			errContains: "fragment",
		},
		{
			name:        "user info rejected",
			input:       "http://user:pass@example.com",
			wantErr:     true,
			errContains: "user info",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b, err := NewBackend(tc.input)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				if b != nil {
					t.Errorf("expected nil backend on error, got %+v", b)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if b == nil {
				t.Fatal("expected non-nil backend")
			}
			if b.URL == nil {
				t.Error("backend.URL is nil")
			}
			if b.ReverseProxy == nil {
				t.Error("backend.ReverseProxy is nil")
			}
			if got := b.URL.Scheme; got != tc.wantScheme {
				t.Errorf("scheme: got %q, want %q", got, tc.wantScheme)
			}
			if got := b.URL.Host; got != tc.wantHost {
				t.Errorf("host: got %q, want %q", got, tc.wantHost)
			}
			if !b.IsHealthy() {
				t.Error("new backend should start healthy")
			}
		})
	}
}

func TestBackendStatusRoundTrip(t *testing.T) {
	t.Parallel()

	b, err := NewBackend("http://localhost:8080")
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	if !b.IsHealthy() {
		t.Fatal("expected healthy on construction")
	}

	b.SetStatus(StatusUnhealthy)
	if b.IsHealthy() {
		t.Error("expected unhealthy after SetStatus(StatusUnhealthy)")
	}

	b.SetStatus(StatusHealthy)
	if !b.IsHealthy() {
		t.Error("expected healthy after SetStatus(StatusHealthy)")
	}
}
