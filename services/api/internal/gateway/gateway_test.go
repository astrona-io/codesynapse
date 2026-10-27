package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestForwardPassesCredentialAndStreams(t *testing.T) {
	var got http.Header
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		assert.Equal(t, r.URL.Path, "/v1/messages")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Set-Cookie", "secret=1")
		_, _ = io.WriteString(w, "event: ping\n\n")
	}))
	defer up.Close()

	var meta Meta
	g := NewAnthropic()
	g.Upstream = up.URL
	g.Done = func(_ context.Context, m Meta, _ int, _ time.Duration) { meta = m }
	srv := httptest.NewServer(g.Handler("/v1/messages"))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"model":"m1","stream":true,"tools":[{}]}`))
	req.Header.Set("X-Api-Key", "sk-ant-x")
	req.Header.Set(KeyHeader, "csk_secret")
	req.Header.Set("Cookie", "cs_session=abc")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // read side
	out, _ := io.ReadAll(resp.Body)

	assert.Equal(t, string(out), "event: ping\n\n")
	assert.Equal(t, resp.Header.Get("Set-Cookie"), "")
	assert.Equal(t, got.Get("X-Api-Key"), "sk-ant-x")
	assert.Equal(t, got.Get("Anthropic-Version"), "2023-06-01")
	assert.Equal(t, got.Get(KeyHeader), "")
	assert.Equal(t, got.Get("Cookie"), "")
	assert.Equal(t, gotBody, `{"model":"m1","stream":true,"tools":[{}]}`)
	assert.Equal(t, meta, Meta{Path: "/v1/messages", Model: "m1", Stream: true, Bytes: len(gotBody), Tools: true})
}

func TestNoCredentialIsRefused(t *testing.T) {
	g := NewAnthropic()
	rec := httptest.NewRecorder()
	g.Handler("/v1/messages")(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}")))
	assert.Equal(t, rec.Code, http.StatusUnauthorized)
}

func TestRedirectIsNotFollowed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
	}))
	defer up.Close()
	g := NewAnthropic()
	g.Upstream = up.URL
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{}"))
	r.Header.Set("X-Api-Key", "k")
	g.Handler("/v1/messages")(rec, r)
	assert.Equal(t, rec.Code, http.StatusFound)
}

func TestBodyIsCapped(t *testing.T) {
	g := NewAnthropic()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", io.LimitReader(zeros{}, MaxBody+1))
	r.Header.Set("X-Api-Key", "k")
	g.Handler("/v1/messages")(rec, r)
	assert.Equal(t, rec.Code, http.StatusRequestEntityTooLarge)
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
