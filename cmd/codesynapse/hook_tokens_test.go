package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

const tokenTranscript = `{"type":"assistant","message":{"id":"m1","content":"private code","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":10,"output_tokens":5}}}
`

func TestReadHookTokenCounts(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       usage.TokenCounts
		known      bool
	}{
		{"claude keeps cache reads and writes apart", tokenTranscript, usage.TokenCounts{Input: 360, Output: 30, CachedInput: 200, CacheWrite: 50, Turns: 2, CacheKnownTurns: 1}, true},
		{"claude cache fields of one kind only", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":90}}}`,
			usage.TokenCounts{Input: 100, Output: 1, CachedInput: 90, Turns: 1}, true},
		{"claude zero cache fields are known", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
			usage.TokenCounts{Input: 10, Output: 1, Turns: 1, CacheKnownTurns: 1}, true},
		{"missing usage", `{"type":"user","message":{"content":"private"}}`, usage.TokenCounts{}, false},
		{"invalid counts", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":-1}}}`, usage.TokenCounts{}, false},
		{"codex cumulative", `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":50}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"output_tokens":30,"cached_input_tokens":100}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"output_tokens":30,"cached_input_tokens":100}}}}
`, usage.TokenCounts{Input: 200, Output: 30, CachedInput: 100, Turns: 2, CacheKnownTurns: 2}, true},
		{"codex without cached field", `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10}}}}
`, usage.TokenCounts{Input: 100, Output: 10, Turns: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := scanTokenCounts(strings.NewReader(tc.text))
			assert.Equal(t, known, tc.known)
			assert.Equal(t, got, tc.want)
		})
	}
}

func TestHookTokenReportDeltaAndRetry(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv(apiTokenEnv, "csk_test")
	var reports []usage.TokenCounts
	failed := true
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tokens usage.TokenCounts `json:"tokens"`
		}
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "private code")
		assert.NotContains(t, string(raw), "m1")
		require.NoError(t, json.Unmarshal(raw, &in))
		if failed {
			w.WriteHeader(503)
			return
		}
		reports = append(reports, in.Tokens)
		w.WriteHeader(204)
	}))
	t.Cleanup(api.Close)
	t.Setenv("CODESYNAPSE_API_URL", api.URL)
	transcript := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(tokenTranscript), 0o600))
	in := toolHookInput{SessionID: "token-session", Cwd: gitRepo(t), TranscriptPath: transcript}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reportRetrieval(context.Background(), in, logger)
	assert.Equal(t, loadState(stateFile(in.SessionID)).TokenSent, usage.TokenCounts{}, "failed reports retain pending usage")
	failed = false
	reportRetrieval(context.Background(), in, logger)
	reportRetrieval(context.Background(), in, logger)
	require.Len(t, reports, 1, "repeated Stop does not recount the transcript")
	assert.Equal(t, reports[0], usage.TokenCounts{Input: 360, Output: 30, CachedInput: 200, CacheWrite: 50, Turns: 2, CacheKnownTurns: 1})
	extra := `{"type":"assistant","message":{"id":"m3","usage":{"input_tokens":40,"output_tokens":10,"cache_read_input_tokens":300,"cache_creation_input_tokens":20}}}`
	require.NoError(t, os.WriteFile(transcript, []byte(tokenTranscript+extra+"\n"), 0o600))
	reportRetrieval(context.Background(), in, logger)
	require.Len(t, reports, 2)
	assert.Equal(t, reports[1], usage.TokenCounts{Input: 360, Output: 10, CachedInput: 300, CacheWrite: 20, Turns: 1, CacheKnownTurns: 1})
}

func FuzzHookTokenCounts(f *testing.F) {
	f.Add(tokenTranscript)
	f.Add(`{"type":"assistant"}`)
	f.Add(`{"type":"assistant","message":{"id":"a","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":0}}}`)
	f.Add(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":5,"output_tokens":1,"cached_input_tokens":9}}}}`)
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<20 {
			t.Skip()
		}
		got, known := scanTokenCounts(strings.NewReader(text))
		if known {
			require.NoError(t, got.Validate())
			// Cache reads and writes never overlap and never exceed the input.
			assert.True(t, got.CachedInput+got.CacheWrite <= got.Input)
			assert.True(t, got.CacheKnownTurns <= got.Turns)
		}
	})
}
