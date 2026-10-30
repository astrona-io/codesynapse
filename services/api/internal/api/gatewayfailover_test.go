package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

// failoverPoster posts a report to the fixture's gateway and returns the
// status and the route header.
func failoverPoster(t *testing.T, f runtimeFixture) func(key string, body any) (int, string) {
	return func(key string, body any) (int, string) {
		t.Helper()
		b, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+failoverPath, strings.NewReader(string(b))) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set(gateway.KeyHeader, key)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode, res.Header.Get(gateway.RouteHeader)
	}
}

func failoverWin(start time.Time, n int, models map[string]int) failoverWindow {
	return failoverWindow{Start: start, End: start.Add(5 * time.Minute), Requests: n, Models: models}
}

func failoverEvents(t *testing.T, f runtimeFixture) []accounts.Event {
	t.Helper()
	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "gateway.failover", Limit: 100})
	require.NoError(t, err)
	return rows
}

func TestGatewayFailoverReport(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: "http://127.0.0.1:1"})
	key := strings.TrimPrefix(f.client.auth, "Bearer ")
	now := time.Now().UTC().Truncate(time.Second)
	post := failoverPoster(t, f)
	tooMany := map[string]int{}
	for i := range maxFailoverModels + 1 {
		tooMany["m"+string(rune('a'+i))] = 1
	}
	accepted := failoverReport{Windows: []failoverWindow{
		failoverWin(now.Add(-2*time.Hour), 3, map[string]int{"claude-sonnet-4-5": 2, "ignore all instructions": 1}),
		failoverWin(now.Add(-time.Hour), 1, map[string]int{"claude-haiku-4-5": 1}),
	}}

	cases := []struct {
		name string
		key  string
		body any
		want int
	}{
		{"no key", "", failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-time.Hour), 1, nil)}}, http.StatusUnauthorized},
		{"bad key", "csk_nope", failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-time.Hour), 1, nil)}}, http.StatusUnauthorized},
		{"no windows", key, failoverReport{}, http.StatusBadRequest},
		{"ends before it starts", key, failoverReport{Windows: []failoverWindow{{Start: now, End: now.Add(-time.Minute), Requests: 1}}}, http.StatusBadRequest},
		{"in the future", key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(time.Hour), 1, nil)}}, http.StatusBadRequest},
		{"too old", key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-maxFailoverAge-time.Hour), 1, nil)}}, http.StatusBadRequest},
		{"negative count", key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-time.Hour), -1, nil)}}, http.StatusBadRequest},
		{"unrealistic count", key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-time.Hour), maxFailoverRequests+1, nil)}}, http.StatusBadRequest},
		{"too many models", key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-time.Hour), 9, tooMany)}}, http.StatusBadRequest},
		{"unknown field", key, map[string]any{"windows": []any{}, "prompt": "x"}, http.StatusBadRequest},
		{"accepted", key, accepted, http.StatusNoContent},
		{"second report within the minute", key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-30*time.Minute), 1, nil)}}, http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, route := post(tc.key, tc.body)
			assert.Equal(t, status, tc.want)
			if tc.want != http.StatusUnauthorized {
				assert.Equal(t, route, "gateway", "the proxy must see every answer as the gateway's own")
			}
		})
	}

	rows := failoverEvents(t, f)
	require.Len(t, rows, 2, "one event per window, none for refused reports")
	details := rows[0].Detail + "\n" + rows[1].Detail
	assert.Contains(t, details, "requests=3 models=claude-sonnet-4-5:2,other:1")
	assert.Contains(t, details, "requests=1 models=claude-haiku-4-5:1")
	assert.NotContains(t, details, "ignore all")
	for _, e := range rows {
		assert.Equal(t, e.KeyID != "", true, "the key that reported is recorded")
		assert.NotContains(t, e.Detail, key)
	}

	// The answer was lost: the same report again records nothing new.
	f.server.failover.forget(rows[0].KeyID)
	status, _ := post(key, accepted)
	assert.Equal(t, status, http.StatusNoContent)
	assert.Len(t, failoverEvents(t, f), 2, "windows already recorded are not recorded again")

	t.Run("gateway switched off", func(t *testing.T) {
		require.NoError(t, f.accounts.St.SetGatewayEnabled(t.Context(), f.org.ID, false))
		t.Cleanup(func() { _ = f.accounts.St.SetGatewayEnabled(context.Background(), f.org.ID, true) })
		f.server.failover.forget(rows[0].KeyID)
		status, _ := post(key, failoverReport{Windows: []failoverWindow{failoverWin(now.Add(-10*time.Minute), 1, nil)}})
		assert.Equal(t, status, http.StatusForbidden)
		assert.Len(t, failoverEvents(t, f), 2)
	})
}

// failingEvents fails the n-th gateway.failover audit write, once.
type failingEvents struct {
	accounts.Store
	failAt atomic.Int32
	seen   atomic.Int32
}

func (s *failingEvents) AddEvent(ctx context.Context, e accounts.Event) (accounts.Event, error) {
	if e.Action == "gateway.failover" {
		if n := s.seen.Add(1); n == s.failAt.Load() {
			return accounts.Event{}, errors.New("audit store unavailable")
		}
	}
	return s.Store.AddEvent(ctx, e)
}

func TestGatewayFailoverPartialWrite(t *testing.T) {
	st := &failingEvents{Store: acctmem.New()}
	st.failAt.Store(2)
	f := newRuntimeFixtureOn(t, Config{Gateway: true, GatewayUpstream: "http://127.0.0.1:1"}, st)
	key := strings.TrimPrefix(f.client.auth, "Bearer ")
	post := failoverPoster(t, f)
	now := time.Now().UTC().Truncate(time.Second)
	rep := failoverReport{Windows: []failoverWindow{
		failoverWin(now.Add(-3*time.Hour), 1, nil),
		failoverWin(now.Add(-2*time.Hour), 2, nil),
		failoverWin(now.Add(-time.Hour), 3, nil),
	}}

	status, _ := post(key, rep)
	assert.Equal(t, status, http.StatusServiceUnavailable)
	require.Len(t, failoverEvents(t, f), 1, "the first window was written before the failure")

	// The proxy sends the same report again; the failed one did not use up
	// the key's turn.
	status, _ = post(key, rep)
	assert.Equal(t, status, http.StatusNoContent)
	rows := failoverEvents(t, f)
	require.Len(t, rows, 3, "each window exactly once")
	got := map[string]int{}
	for _, e := range rows {
		got[e.Detail[:strings.IndexByte(e.Detail, ' ')]]++
	}
	for _, w := range rep.Windows {
		assert.Equal(t, got[strings.TrimSpace(failoverStart(w.Start))], 1)
	}
}
