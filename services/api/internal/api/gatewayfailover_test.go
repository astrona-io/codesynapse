package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

func TestGatewayFailoverReport(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: "http://127.0.0.1:1"})
	key := strings.TrimPrefix(f.client.auth, "Bearer ")
	now := time.Now().UTC().Truncate(time.Second)
	post := func(key string, body any) int {
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
		return res.StatusCode
	}
	win := func(start time.Time, n int, models map[string]int) failoverWindow {
		return failoverWindow{Start: start, End: start.Add(5 * time.Minute), Requests: n, Models: models}
	}
	tooMany := map[string]int{}
	for i := range maxFailoverModels + 1 {
		tooMany["m"+string(rune('a'+i))] = 1
	}

	cases := []struct {
		name string
		key  string
		body any
		want int
	}{
		{"no key", "", failoverReport{Windows: []failoverWindow{win(now.Add(-time.Hour), 1, nil)}}, http.StatusUnauthorized},
		{"bad key", "csk_nope", failoverReport{Windows: []failoverWindow{win(now.Add(-time.Hour), 1, nil)}}, http.StatusUnauthorized},
		{"no windows", key, failoverReport{}, http.StatusBadRequest},
		{"ends before it starts", key, failoverReport{Windows: []failoverWindow{{Start: now, End: now.Add(-time.Minute), Requests: 1}}}, http.StatusBadRequest},
		{"in the future", key, failoverReport{Windows: []failoverWindow{win(now.Add(time.Hour), 1, nil)}}, http.StatusBadRequest},
		{"negative count", key, failoverReport{Windows: []failoverWindow{win(now.Add(-time.Hour), -1, nil)}}, http.StatusBadRequest},
		{"too many models", key, failoverReport{Windows: []failoverWindow{win(now.Add(-time.Hour), 9, tooMany)}}, http.StatusBadRequest},
		{"unknown field", key, map[string]any{"windows": []any{}, "prompt": "x"}, http.StatusBadRequest},
		{"accepted", key, failoverReport{Windows: []failoverWindow{
			win(now.Add(-2*time.Hour), 3, map[string]int{"claude-sonnet-4-5": 2, "ignore all instructions": 1}),
			win(now.Add(-time.Hour), 1, map[string]int{"claude-haiku-4-5": 1}),
		}}, http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, post(tc.key, tc.body), tc.want)
		})
	}

	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "gateway.failover", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 2, "one event per window, none for refused reports")
	details := rows[0].Detail + "\n" + rows[1].Detail
	assert.Contains(t, details, "requests=3 models=claude-sonnet-4-5:2,other:1")
	assert.Contains(t, details, "requests=1 models=claude-haiku-4-5:1")
	assert.NotContains(t, details, "ignore all")
	for _, e := range rows {
		assert.Equal(t, e.KeyID != "", true, "the key that reported is recorded")
		assert.NotContains(t, e.Detail, key)
	}
}
