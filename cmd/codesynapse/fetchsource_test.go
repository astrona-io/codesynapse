package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/fetchsource"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The bridge answers fetch_source itself and reports the call without the
// URL, its host or the checkout's name. Fetching itself is covered in
// internal/fetchsource; here the dial guard refuses the address, so no
// network is needed.
func TestBridgeAnswersFetchSourceAndReportsItWithoutTheURL(t *testing.T) {
	var mu sync.Mutex
	var reports []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, r.URL.Path, "/api/v1/usage/local")
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		reports = append(reports, string(raw))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(api.Close)
	var out bytes.Buffer
	b := &bridge{token: "csk_test", out: &out, repoName: "acme/app", fetcher: fetchsource.New(nil),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), report: apiClient{base: api.URL, token: "csk_test", http: api.Client()}}
	b.repoOnce.Do(func() {})

	call := func(args string) map[string]any {
		out.Reset()
		msg := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fetch_source","arguments":` + args + `}}`
		var m rpcMessage
		require.NoError(t, json.Unmarshal([]byte(msg), &m))
		require.True(t, b.answerFetchSource(context.Background(), []byte(msg), m))
		var res struct {
			Result map[string]any `json:"result"`
		}
		require.NoError(t, json.Unmarshal(out.Bytes(), &res))
		return res.Result
	}
	refused := call(`{"url":"https://169.254.169.254/latest/meta-data/"}`)
	assert.Equal(t, refused["isError"], true)
	assert.Contains(t, refused["content"].([]any)[0].(map[string]any)["text"].(string), "not on the public internet")
	invalid := call(`{"url":"http://docs.example.com/secret-page"}`)
	assert.Equal(t, invalid["isError"], true)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, reports, 2)
	for _, r := range reports {
		var u localUsage
		require.NoError(t, json.Unmarshal([]byte(r), &u))
		assert.Equal(t, u.Tool, fetchsource.Tool)
		assert.True(t, u.Failed)
		assert.Equal(t, u.Repo, "", "a fetched page is not tied to the checkout")
		for _, leak := range []string{"169.254", "example.com", "secret-page", "meta-data"} {
			assert.NotContains(t, r, leak)
		}
	}

	other := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search","arguments":{}}}`
	var m rpcMessage
	require.NoError(t, json.Unmarshal([]byte(other), &m))
	assert.False(t, b.answerFetchSource(context.Background(), []byte(other), m), "other tools go to the server")
}
