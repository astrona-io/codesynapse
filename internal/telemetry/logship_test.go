package telemetry

import (
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

func TestLogShipperSendsJSONLines(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
		query  string
		user   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u, _, _ := r.BasicAuth()
		mu.Lock()
		bodies, query, user = append(bodies, string(b)), r.URL.Path+"?"+r.URL.RawQuery, u
		mu.Unlock()
	}))
	defer srv.Close()

	s := &LogShipper{URL: srv.URL + "/", User: "codesynapse", Password: "secret", StreamFields: []string{"service", "instance"}}
	log := slog.New(slog.NewJSONHandler(s, nil)).With("service", "codesynapse-api")
	log.Info("first", "n", 1)
	log.Warn("second")
	require.NoError(t, s.Flush(context.Background()))
	require.NoError(t, s.Flush(context.Background())) // nothing left: no request

	require.Len(t, bodies, 1)
	lines := strings.Split(strings.TrimSuffix(bodies[0], "\n"), "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], `"msg":"first"`)
	assert.Contains(t, lines[1], `"level":"WARN"`)
	assert.Contains(t, query, "/insert/jsonline?")
	assert.Contains(t, query, "_stream_fields=service%2Cinstance")
	assert.Contains(t, query, "_msg_field=msg")
	assert.Equal(t, user, "codesynapse")
}

func TestLogShipperBounds(t *testing.T) {
	t.Run("long lines are dropped", func(t *testing.T) {
		s := &LogShipper{}
		n, err := s.Write(make([]byte, logLineBytes+1))
		assert.NoError(t, err)
		assert.Equal(t, n, logLineBytes+1)
		assert.Equal(t, s.size, 0)
	})
	t.Run("a full queue drops instead of blocking", func(t *testing.T) {
		s := &LogShipper{}
		line := []byte(strings.Repeat("x", logLineBytes-1) + "\n")
		for range logQueueBytes/len(line) + 10 {
			_, err := s.Write(line)
			assert.NoError(t, err)
		}
		assert.LessOrEqual(t, s.size, logQueueBytes)
	})
	t.Run("batches stay under the batch size", func(t *testing.T) {
		s := &LogShipper{}
		line := []byte(strings.Repeat("x", 1000) + "\n")
		for range 3 * logBatchBytes / len(line) {
			_, _ = s.Write(line)
		}
		for b := s.take(); len(b) > 0; b = s.take() {
			assert.LessOrEqual(t, len(b), logBatchBytes)
		}
		assert.Equal(t, s.size, 0)
	})
}

func TestLogShipperFailedBatchIsDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := &LogShipper{URL: srv.URL}
	_, _ = s.Write([]byte(`{"msg":"x"}` + "\n"))
	err := s.Flush(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 401")
	assert.Equal(t, s.size, 0)
	assert.Len(t, s.queue, 0)
}
