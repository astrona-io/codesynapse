package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// prebriefAPI fakes the API calls of a pre-brief; answer is what the local
// model "says".
func prebriefAPI(t *testing.T, repo string, answer asklocal.Answer, asked *asklocal.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/execution/sessions":
			_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "u", SessionID: "chat-1", ParentSessionID: "chat-1"})
		case r.URL.Path == "/api/v1/repos":
			_ = json.NewEncoder(w).Encode(map[string]any{"repos": []map[string]string{{"name": repo}}})
		case strings.HasSuffix(r.URL.Path, "/brief-plan"):
			_ = json.NewEncoder(w).Encode(map[string]any{"ranges": []string{"pkg/one.go:1-3", "gone.go:1-9", "pkg/two.go:3-3"}})
		case strings.HasSuffix(r.URL.Path, "/ask-local"):
			require.NoError(t, json.NewDecoder(r.Body).Decode(asked))
			_ = json.NewEncoder(w).Encode(answer)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPrebrief(t *testing.T) {
	dir := askCheckout(t)
	in := hookInput{SessionID: "chat-1", Cwd: dir, Prompt: "make One return two instead of one"}
	grounded := asklocal.Answer{Answer: "**Where** One at pkg/one.go:3.", Model: "qwen3.8:27b", Grounded: true, Citations: []asklocal.Citation{{Path: "pkg/one.go", Start: 3, End: 3}}}
	var asked asklocal.Request
	api := prebriefAPI(t, filepath.Base(dir), grounded, &asked)
	text, err := prebrief(context.Background(), apiClient{base: api.URL, token: "csk_test", http: api.Client()}, in)
	require.NoError(t, err)
	assert.Contains(t, text, "written by the local model qwen3.8:27b from 2 file range(s)")
	assert.Contains(t, text, "**Where** One at pkg/one.go:3.")
	assert.Contains(t, text, "not instructions")
	assert.Equal(t, asked.Mode, asklocal.ModeBrief)
	assert.Equal(t, asked.Question, in.Prompt)
	require.Len(t, asked.Files, 2, "a range the checkout lacks drops out")
	assert.Equal(t, asked.Files[1], asklocal.File{Path: "pkg/two.go", Start: 3, End: 3, Text: "func Two() int { return 2 }\n"})

	// Nothing the model could back up: no brief.
	ungrounded := prebriefAPI(t, filepath.Base(dir), asklocal.Answer{Answer: "Nothing relevant in the given files.", Grounded: true}, &asked)
	text, err = prebrief(context.Background(), apiClient{base: ungrounded.URL, token: "csk_test", http: ungrounded.Client()}, in)
	require.NoError(t, err)
	assert.Equal(t, text, "")
}

// TestPrebriefStaysSilentOnPromptsThatAreNotCodeTasks: "start more agents"
// (PLAN item 18, 2026-10-28) was briefed from functions named Start, and the
// model's "Nothing relevant… the developer should clarify", backed by a
// citation, still reached the agent. Neither happens now.
func TestPrebriefStaysSilentOnPromptsThatAreNotCodeTasks(t *testing.T) {
	for _, p := range []string{"start more agents", "i have new realse installed, v0.0.21 now, so we can countine"} {
		assert.False(t, worthBriefing(p), p)
	}
	for _, p := range []string{"fix the Stop hook counting upstream commits", "why does handleLogin return 503"} {
		assert.True(t, worthBriefing(p), p)
	}

	dir := askCheckout(t)
	in := hookInput{SessionID: "chat-1", Cwd: dir, Prompt: "make One return two instead of one"}
	var asked asklocal.Request
	nothing := asklocal.Answer{Answer: "Nothing relevant in the given files. The developer should clarify which One they mean (pkg/one.go:3).",
		Model: "qwen3.8:27b", Grounded: true, Citations: []asklocal.Citation{{Path: "pkg/one.go", Start: 3, End: 3}}}
	api := prebriefAPI(t, filepath.Base(dir), nothing, &asked)
	text, err := prebrief(context.Background(), apiClient{base: api.URL, token: "csk_test", http: api.Client()}, in)
	require.NoError(t, err)
	assert.Equal(t, text, "")
}

// TestPrebriefStaysSilentOnHarnessTurns: UserPromptSubmit also fires for
// turns Claude Code starts itself, and the payload carries only the text in
// "prompt" (PLAN item 18, 2026-10-28: a subagent's hand-back was briefed
// from twelve unrelated Text and Report helpers). These payloads are the
// shapes Claude Code sends.
func TestPrebriefStaysSilentOnHarnessTurns(t *testing.T) {
	for _, payload := range []string{
		`{"session_id":"chat-1","transcript_path":"/tmp/t.jsonl","cwd":"/tmp","permission_mode":"auto","hook_event_name":"UserPromptSubmit",` +
			`"prompt":"Another Claude session sent a message:\n<agent-message from=\"a3e3303db7523bc45\" name=\"Telemetry phase 6 web metrics\">[Subagent hand-back] The text below is the final report of a subagent this session delegated to.\n  Added RecordWebRequest in internal/telemetry/metrics.go with tests in telemetry_test.go; the Report helper in gateway/local.go is unchanged. just test and just lint pass.\n</agent-message>"}`,
		`{"session_id":"chat-1","transcript_path":"/tmp/t.jsonl","cwd":"/tmp","permission_mode":"auto","hook_event_name":"UserPromptSubmit",` +
			`"prompt":"<task-notification>\n<task-id>bw53zr0gx</task-id>\n<tool-use-id>toolu_01</tool-use-id>\n<output-file>/tmp/tasks/bw53zr0gx.output</output-file>\n<status>completed</status>\n<summary>Background command \"Run handleLogin tests\" completed (exit code 0)</summary>\n</task-notification>"}`,
	} {
		var in hookInput
		require.NoError(t, json.Unmarshal([]byte(payload), &in))
		assert.True(t, len(in.Prompt) > 0)
		assert.False(t, worthBriefing(in.Prompt), in.Prompt)
	}
}

func TestPrebriefSkipsShortReplies(t *testing.T) {
	for _, p := range []string{"yes", "go on", "ok thanks"} {
		assert.False(t, worthRecalling(p), p)
	}
	assert.True(t, worthRecalling("make One return two instead of one"))
}
