package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestStartLogs(t *testing.T) {
	var (
		mu   sync.Mutex
		got  strings.Builder
		auth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, pw, _ := r.BasicAuth()
		mu.Lock()
		got.Write(b)
		auth = pw
		mu.Unlock()
	}))
	defer srv.Close()
	t.Setenv(EnvLogsURL, srv.URL)
	t.Setenv(EnvLogsUser, "codesynapse")
	t.Setenv(EnvLogsPassword, "pw")
	t.Setenv(EnvEnvironment, "test")

	var stderr bytes.Buffer
	env := &Env{level: new(slog.LevelVar)}
	env.Logger = slog.New(traceHandler{slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: env.level})})
	ctx, stop, err := StartLogs(context.Background(), env, "api", "v1")
	require.NoError(t, err)
	env.Logger.InfoContext(ctx, "hello", "n", 1)
	env.Logger.Debug("hidden") // below the level: neither copy
	stop()
	stop() // idempotent

	assert.Contains(t, stderr.String(), "msg=hello")
	assert.NotContains(t, stderr.String(), "hidden")
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, got.String(), `"msg":"hello"`)
	assert.Contains(t, got.String(), `"service":"codesynapse-api"`)
	assert.Contains(t, got.String(), `"environment":"test"`)
	assert.Contains(t, got.String(), `"version":"v1"`)
	assert.NotContains(t, got.String(), "hidden")
	assert.Equal(t, auth, "pw")
}

func TestStartLogsOff(t *testing.T) {
	t.Setenv(EnvLogsURL, "")
	env := &Env{level: new(slog.LevelVar), Logger: slog.New(slog.DiscardHandler)}
	before := env.Logger
	_, stop, err := StartLogs(context.Background(), env, "mcp")
	require.NoError(t, err)
	stop()
	assert.True(t, env.Logger == before)

	t.Setenv(EnvLogsURL, "http://user:pw@logs:9428")
	_, _, err = StartLogs(context.Background(), env, "mcp")
	assert.Error(t, err)
}
