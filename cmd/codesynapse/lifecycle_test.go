package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLifecycleRuntimeIdentityAndMetadata(t *testing.T) {
	for _, tc := range []struct{ client, event, payload string }{
		{"codex", "PostToolUse", `{"session_id":"chat-a","tool_name":"Bash","tool_input":{"command":"secret source"},"tool_response":"private output","duration_ms":42}`},
		{"cursor", "postToolUse", `{"conversation_id":"chat-a","tool_name":"Shell","tool_input":{"command":"secret source"},"tool_output":"private output","duration_ms":42}`},
		{"cursor", "afterShellExecution", `{"conversation_id":"chat-a","command":"secret source","output":"private output","duration":42,"sandbox":false}`},
	} {
		t.Run(tc.client, func(t *testing.T) {
			_, tool, a, err := decodeLifecycle(strings.NewReader(tc.payload), tc.client, tc.event)
			require.NoError(t, err)
			assert.Equal(t, tool.SessionID, "chat-a")
			assert.Equal(t, a.Category, "shell")
			require.NotNil(t, a.DurationMS)
			assert.Equal(t, *a.DurationMS, int64(42))
			body, err := json.Marshal(a)
			require.NoError(t, err)
			assert.False(t, bytes.Contains(body, []byte("secret")))
			assert.False(t, bytes.Contains(body, []byte("private")))
			assert.True(t, a.InputBytes > 0 && a.OutputBytes > 0)
		})
	}
	for _, payload := range []string{`{}`, `{"session_id":"../bad"}`, `{"session_id":"one","conversation_id":"two"}`, `{"session_id":"one","parent_conversation_id":"two"}`} {
		_, _, _, err := decodeLifecycle(strings.NewReader(payload), "cursor", "preToolUse")
		require.Error(t, err)
	}
}

func TestCursorNativeCompactionAndMCPFields(t *testing.T) {
	_, _, a, err := decodeLifecycle(strings.NewReader(`{"conversation_id":"chat-a","trigger":"auto","context_usage_percent":85,"context_tokens":120000,"context_window_size":128000,"message_count":45,"messages_to_compact":30,"is_first_compaction":true}`), "cursor", "preCompact")
	require.NoError(t, err)
	assert.Equal(t, *a.ContextTokens, int64(120000))
	assert.Equal(t, *a.ContextPercent, float64(85))
	assert.Equal(t, *a.MessagesToCompact, int64(30))
	_, tool, a, err := decodeLifecycle(strings.NewReader(`{"conversation_id":"chat-a","mcp_server_name":"codesynapse","tool_name":"minimal_context","tool_input":"{\"repo\":\"private/repo\"}","result_json":"{\"text\":\"private.go:1-20\"}","duration":42}`), "cursor", "afterMCPExecution")
	require.NoError(t, err)
	assert.Equal(t, a.Tool, "minimal_context")
	assert.Equal(t, tool.ToolInput.Repo, "private/repo")
	assert.Equal(t, *a.DurationMS, int64(42))
	assert.True(t, a.OutputBytes > 0)
	encoded, err := json.Marshal(a)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(encoded, []byte("private")))
}

func TestEveryLifecycleEventRecordsWithRegisteredHeaders(t *testing.T) {
	t.Setenv(apiTokenEnv, "csk_fixture")
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	for _, client := range []string{"claude", "codex", "cursor"} {
		for _, event := range agentcfg.HookEvents(client) {
			t.Run(client+"/"+event, func(t *testing.T) {
				var activity agentcfg.HookActivity
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/execution/sessions":
						_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "tenant", SubjectID: "user", SessionID: "chat-a", ParentSessionID: "chat-a"})
					case "/api/v1/execution/activity":
						assert.Equal(t, r.Header.Get(execution.SessionHeader), "chat-a")
						assert.Equal(t, r.Header.Get(execution.ParentSessionHeader), "chat-a")
						assert.Equal(t, r.Header.Get("Authorization"), "Bearer csk_fixture")
						require.NoError(t, json.NewDecoder(r.Body).Decode(&activity))
						_, _ = w.Write([]byte(`{}`))
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(api.Close)
				var output bytes.Buffer
				o := &lifecycleOptions{client: client, event: event, apiURL: api.URL, EC: &app.Env{Stdin: strings.NewReader(`{"session_id":"chat-a"}`), Stdout: &output, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}}
				require.NoError(t, o.Run(t.Context(), nil))
				assert.Equal(t, activity.Client, client)
				assert.Equal(t, activity.Event, event)
				assert.True(t, json.Valid(output.Bytes()))
				if event == "PermissionRequest" {
					assert.Equal(t, output.String(), "{}\n", "never auto-approve permissions")
				}
			})
		}
	}
}

func FuzzLifecycleInput(f *testing.F) {
	f.Add(`{"conversation_id":"chat-a","tool_input":"{}"}`)
	f.Add(`{"session_id":"chat-a","duration_ms":-1}`)
	f.Fuzz(func(t *testing.T, input string) {
		_, _, a, err := decodeLifecycle(strings.NewReader(input), "cursor", "postToolUse")
		if err == nil {
			require.NoError(t, a.Validate())
		}
	})
}

func TestClaudeActivityMeasuresWithoutContent(t *testing.T) {
	file := filepath.Join(t.TempDir(), "CLAUDE.md")
	require.NoError(t, os.WriteFile(file, []byte("secret rules"), 0o600))
	raw := `{"session_id":"chat-a","file_path":` + strconv.Quote(file) + `,"reason":"logout"}`
	_, _, a, err := decodeLifecycle(strings.NewReader(raw), "claude", "InstructionsLoaded")
	require.NoError(t, err)
	assert.Equal(t, a.ContentBytes, int64(len("secret rules")))
	encoded, err := json.Marshal(a)
	require.NoError(t, err)
	assert.False(t, bytes.Contains(encoded, []byte("CLAUDE.md")), "the path is not reported")
	_, _, a, err = decodeLifecycle(strings.NewReader(raw), "claude", "SessionEnd")
	require.NoError(t, err)
	assert.Equal(t, a.Source, "other", "reasons outside the enum are folded")
	_, _, a, err = decodeLifecycle(strings.NewReader(`{"session_id":"chat-a"}`), "claude", "PermissionDenied")
	require.NoError(t, err)
	assert.Equal(t, a.FailureType, "permission_denied")
}

func TestPluginHookReportsActivityOnce(t *testing.T) {
	t.Setenv(apiTokenEnv, "csk_fixture")
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var registered, activity int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/execution/sessions":
			registered++
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "tenant", SubjectID: "user", SessionID: "chat-a", ParentSessionID: "chat-a"})
		case "/api/v1/execution/activity":
			activity++
			assert.Equal(t, r.Header.Get(execution.SessionHeader), "chat-a")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	t.Setenv("CODESYNAPSE_API_URL", api.URL)
	t.Setenv("CODESYNAPSE_HOOK_ACTIVITY", "true")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	payload := `{"session_id":"chat-a","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"git status"}}`
	for range 3 {
		o := &hookOptions{event: "bash", EC: &app.Env{Stdin: strings.NewReader(payload), Stdout: io.Discard, Logger: logger}}
		require.NoError(t, o.Run(t.Context(), nil))
	}
	assert.Equal(t, registered, 1, "the session is registered once")
	assert.Equal(t, activity, 3)
	t.Setenv("CODESYNAPSE_HOOK_ACTIVITY", "false")
	o := &hookOptions{event: "bash", EC: &app.Env{Stdin: strings.NewReader(payload), Stdout: io.Discard, Logger: logger}}
	require.NoError(t, o.Run(t.Context(), nil))
	assert.Equal(t, activity, 3, "switched off")
}
