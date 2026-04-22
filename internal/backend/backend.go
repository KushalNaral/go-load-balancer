package backend

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sync/atomic"
)

// The status is written by a writer ( a health loop )
// The status is then read by a reader ( a serve loop )

type Status int32

var allowedSchemes = []string{"http", "https"}

const (
	StatusHealthy Status = iota
	StatusUnhealthy
)

// Status is a typed enum, not a bool. Future states (draining, probation) slot in without a rewrite
// Reads via atomic.Load; writes only from the health loop.
type Backend struct {
	URL          *url.URL
	Status       atomic.Int32
	ReverseProxy *httputil.ReverseProxy
}

func NewBackend(rawURL string) (*Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse backend url %q: %w", rawURL, err)
	}

	// - Scheme must be exactly http or https.
	// - Host must be non-empty.
	// - Path must be empty or /.
	// - No query, no fragment, no user info.

	if u.Scheme == "" {
		return nil, fmt.Errorf("backend url %q has no scheme (expected http or https)", rawURL)
	}

	if !slices.Contains(allowedSchemes, u.Scheme) {
		return nil, fmt.Errorf("backend url scheme %q not allowed (must be http or https)", u.Scheme)
	}

	if u.Path != "/" && u.Path != "" {
		return nil, fmt.Errorf("backend url %q must not have a path (got %q)", rawURL, u.Path)
	}

	if u.Host == "" {
		return nil, fmt.Errorf("backend url %q has no host", rawURL)
	}

	if u.RawQuery != "" {
		return nil, fmt.Errorf("backend url must not have a query string (got %q)", u.RawQuery)
	}

	if u.Fragment != "" {
		return nil, fmt.Errorf("backend url must not have a fragment (got %q)", u.Fragment)
	}
	if u.User != nil {
		return nil, fmt.Errorf("backend url must not contain user info")
	}

	backend := &Backend{
		URL:          u,
		ReverseProxy: httputil.NewSingleHostReverseProxy(u),
	}

	backend.Status.Store(int32(StatusHealthy))
	return backend, nil
}

func (b *Backend) IsHealthy() bool {
	s := b.Status.Load()
	return s == int32(StatusHealthy)
}

func (b *Backend) SetStatus(s Status) {
	b.Status.Store(int32(s))
}

var _ http.Handler = (*Backend)(nil)

func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.ReverseProxy.ServeHTTP(w, r)
}
