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
		{"claude duplicate blocks", tokenTranscript, usage.TokenCounts{Input: 360, Output: 30, CachedInput: 200, Turns: 2}, true},
		{"missing usage", `{"type":"user","message":{"content":"private"}}`, usage.TokenCounts{}, false},
		{"invalid counts", `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":-1}}}`, usage.TokenCounts{}, false},
		{"codex cumulative", `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":10,"cached_input_tokens":50}}}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":200,"output_tokens":30,"cached_input_tokens":100}}}}
`, usage.TokenCounts{Input: 200, Output: 30, CachedInput: 100}, true},
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
	assert.Equal(t, reports[0], usage.TokenCounts{Input: 360, Output: 30, CachedInput: 200, Turns: 2})
	extra := `{"type":"assistant","message":{"id":"m3","usage":{"input_tokens":40,"output_tokens":10}}}`
	require.NoError(t, os.WriteFile(transcript, []byte(tokenTranscript+extra+"\n"), 0o600))
	reportRetrieval(context.Background(), in, logger)
	require.Len(t, reports, 2)
	assert.Equal(t, reports[1], usage.TokenCounts{Input: 40, Output: 10, Turns: 1})
}

func FuzzHookTokenCounts(f *testing.F) {
	f.Add(tokenTranscript)
	f.Add(`{"type":"assistant"}`)
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<20 {
			t.Skip()
		}
		got, known := scanTokenCounts(strings.NewReader(text))
		if known {
			require.NoError(t, got.Validate())
		}
	})
}
