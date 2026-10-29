package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func server(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		ok   bool
	}{
		{"healthy", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/status" {
				w.WriteHeader(http.StatusNotFound)
			}
		}, true},
		{"unhealthy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, false},
		{"redirect is not followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := probe(server(t, tc.h))
			assert.Equal(t, err == nil, tc.ok, "probe result")
		})
	}
	t.Run("nothing listening", func(t *testing.T) {
		assert.True(t, probe("127.0.0.1:1") != nil, "a closed port fails")
	})
	t.Run("not host:port", func(t *testing.T) {
		assert.True(t, probe("http://127.0.0.1:19559/status") != nil, "a URL is refused")
	})
}
