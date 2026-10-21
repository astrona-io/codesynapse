package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
)

func TestIsLoopback(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true, "localhost:1": true, "[::1]:80": true,
		":8080": false, "0.0.0.0:80": false, "10.0.0.1:80": false, "bad": false,
	} {
		assert.Equal(t, IsLoopback(addr), want, addr)
	}
}

func TestRequireBearer(t *testing.T) {
	h := RequireBearer("s3cret", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	tests := []struct {
		header string
		want   int
	}{
		{"", 401}, {"Bearer wrong", 401}, {"s3cret", 401}, {"Bearer s3cret", 204},
	}
	for _, tt := range tests {
		r := httptest.NewRequest("GET", "/", nil)
		if tt.header != "" {
			r.Header.Set("Authorization", tt.header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, w.Code, tt.want, tt.header)
	}
}
