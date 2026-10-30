package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
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

// Tier 2: a long pruned page and a question go to source-extract; any
// failure keeps the pruned page and says why.
func TestBridgeExtractsLongPages(t *testing.T) {
	var mu sync.Mutex
	var asked fetchsource.ExtractRequest
	status, reply := http.StatusOK, fetchsource.Extract{Found: true, Text: "Use `NewClient`.", Model: "qwen", Dropped: 1, Problems: []string{"1 code snippets the page does not contain were dropped"}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/execution/sessions":
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "u", SessionID: "chat-1", ParentSessionID: "chat-1"})
		case "/api/v1/repos/acme%2Fapp/source-extract", "/api/v1/repos/acme/app/source-extract":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&asked))
			if status != http.StatusOK {
				http.Error(w, `{"detail":"this task runs only on a local model and none is configured"}`, status)
				return
			}
			_ = json.NewEncoder(w).Encode(reply)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(api.Close)
	b := &bridge{token: "csk_test", session: "chat-1", repoName: "acme/app", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		report: apiClient{base: api.URL, token: "csk_test", http: api.Client()}}
	b.repoOnce.Do(func() {})
	long := "# Client\n\nUse `NewClient`.\n\n" + strings.Repeat("Some prose about other things. ", 300)
	page := func(text, ct string) *fetchsource.Out {
		return &fetchsource.Out{ContentType: ct, Text: text, TextBytes: len(text)}
	}

	ans := page(long, "text/html")
	b.extractSource(context.Background(), nil, "how do I connect", ans)
	assert.True(t, ans.Extracted)
	assert.Equal(t, ans.Text, "Use `NewClient`.")
	assert.Equal(t, ans.TextBytes, len(ans.Text))
	assert.Equal(t, ans.ExtractModel, "qwen")
	assert.Equal(t, ans.ExtractDropped, 1)
	assert.Contains(t, ans.ExtractNote, "dropped")
	mu.Lock()
	assert.Equal(t, asked.Question, "how do I connect")
	assert.Equal(t, asked.Text, long)
	asked = fetchsource.ExtractRequest{}
	mu.Unlock()

	for name, tc := range map[string]struct {
		ans      *fetchsource.Out
		question string
	}{
		"no question": {page(long, "text/html"), ""},
		"short page":  {page("# Client\n\nshort", "text/html"), "how"},
		"json":        {page(long, "application/json"), "how"},
	} {
		b.extractSource(context.Background(), nil, tc.question, tc.ans)
		assert.False(t, tc.ans.Extracted, name)
		assert.Equal(t, tc.ans.ExtractNote, "", name)
	}
	mu.Lock()
	assert.Equal(t, asked.Text, "", "nothing was sent for pages tier 2 skips")
	mu.Unlock()

	big := page(strings.Repeat("a", fetchsource.MaxExtractInput+1), "text/plain")
	b.extractSource(context.Background(), nil, "how", big)
	assert.False(t, big.Extracted)
	assert.Contains(t, big.ExtractNote, "over")

	t.Setenv("CODESYNAPSE_SOURCE_EXTRACT", "false")
	off := page(long, "text/html")
	b.extractSource(context.Background(), nil, "how", off)
	assert.False(t, off.Extracted)
	t.Setenv("CODESYNAPSE_SOURCE_EXTRACT", "true")

	mu.Lock()
	status = http.StatusConflict
	mu.Unlock()
	noModel := page(long, "text/html")
	b.extractSource(context.Background(), nil, "how", noModel)
	assert.False(t, noModel.Extracted)
	assert.Equal(t, noModel.Text, long, "the pruned page stands")
	assert.Contains(t, noModel.ExtractNote, "not shortened by a local model")

	mu.Lock()
	status, reply = http.StatusOK, fetchsource.Extract{Found: false}
	mu.Unlock()
	notFound := page(long, "text/html")
	b.extractSource(context.Background(), nil, "how", notFound)
	assert.False(t, notFound.Extracted)
	assert.Contains(t, notFound.ExtractNote, "found no answer")

	mu.Lock()
	reply = fetchsource.Extract{Found: true, Text: "key AKIAIOSFODNN7EXAMPLE " + strings.Repeat("x", 10)}
	mu.Unlock()
	secret := page(long, "text/html")
	b.extractSource(context.Background(), nil, "how", secret)
	assert.True(t, secret.Extracted)
	assert.NotContains(t, secret.Text, "AKIA", "the bridge redacts the extract again")

	unindexed := &bridge{logger: b.logger}
	unindexed.repoOnce.Do(func() {})
	u := page(long, "text/html")
	unindexed.extractSource(context.Background(), nil, "how", u)
	assert.Contains(t, u.ExtractNote, "not indexed")
}
