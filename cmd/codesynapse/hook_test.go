package main

import (
	"bytes"
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

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestReadGuard(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	dir := t.TempDir()
	var src strings.Builder
	src.WriteString("package big\n\ntype Store struct{}\n\n")
	for i := range 200 {
		src.WriteString("// F does a thing.\nfunc (s *Store) F" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "() {\n}\n\n")
	}
	big := filepath.Join(dir, "big.go")
	require.NoError(t, os.WriteFile(big, []byte(src.String()), 0o600))
	small := filepath.Join(dir, "small.go")
	require.NoError(t, os.WriteFile(small, []byte("package big\n"), 0o600))

	read := func(file string, offset int) map[string]any {
		var in toolHookInput
		in.SessionID, in.ToolInput.FilePath, in.ToolInput.Offset = "chat-1", file, offset
		return readGuard(in)
	}
	out := read(big, 0)
	require.True(t, out != nil, "the first whole read of a large file is turned back")
	spec := out["hookSpecificOutput"].(map[string]any)
	assert.Equal(t, spec["permissionDecision"], "deny")
	reason := spec["permissionDecisionReason"].(string)
	assert.Contains(t, reason, "type Store: lines 3-3")
	assert.Contains(t, reason, "func (*Store).FAa: lines 6-7")
	assert.Contains(t, reason, "read it again")
	assert.Contains(t, reason, "call ask_local", "a local model can read it instead")
	assert.True(t, read(big, 0) == nil, "reading it again is allowed")
	assert.True(t, read(big, 100) == nil, "a ranged read is never turned back")
	assert.True(t, read(small, 0) == nil, "small files are read as asked")

	t.Setenv("CODESYNAPSE_READ_GUARD_LINES", "0")
	var in toolHookInput
	in.SessionID, in.ToolInput.FilePath = "chat-2", big
	assert.True(t, readGuard(in) == nil, "0 turns the guard off")
}

// offlineAPI points the hooks at a closed port, so a developer's own token and
// server cannot change what a test sees.
func offlineAPI(t *testing.T) {
	t.Helper()
	t.Setenv("CODESYNAPSE_API_URL", "http://127.0.0.1:1")
	t.Setenv(apiTokenEnv, "tok")
}

func TestStopGuard(t *testing.T) {
	offlineAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	in := toolHookInput{SessionID: "chat-1", Cwd: dir}
	assert.True(t, stopGuard(ctx, in, logger) == nil, "a clean tree finishes")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Changed() {}\n"), 0o600))
	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil)
	assert.Equal(t, out["decision"], "block")
	assert.Contains(t, out["reason"].(string), "verify_plan")
	assert.True(t, stopGuard(ctx, in, logger) == nil, "asked once per state of the tree")
	in.StopHookActive = true
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc ChangedAgain() {}\n"), 0o600))
	assert.True(t, stopGuard(ctx, in, logger) == nil, "never while already continuing because of a stop hook")
}

func TestStopGuardSkipsUnindexedCheckout(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var repos, reported, jobs string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/usage/unindexed" {
			b, _ := io.ReadAll(r.Body) //nolint:errcheck // test server
			reported = string(b)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/api/v1/execution/sessions" {
			var body struct {
				SessionID string `json:"session_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)                                                                                                                //nolint:errcheck // test server
			_ = json.NewEncoder(w).Encode(map[string]string{"tenant_id": "t", "subject_id": "s", "session_id": body.SessionID, "parent_session_id": body.SessionID}) //nolint:errcheck // test server
			return
		}
		if r.URL.Path == "/api/v1/jobs" {
			_, _ = io.WriteString(w, jobs) //nolint:errcheck // test server
			return
		}
		_, _ = io.WriteString(w, repos) //nolint:errcheck // test server
	}))
	defer ts.Close()
	t.Setenv("CODESYNAPSE_API_URL", ts.URL)
	t.Setenv(apiTokenEnv, "tok")
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	dir := gitRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Changed() {}\n"), 0o600))
	repos = `{"repos":[{"name":"astrona-io/other","remote":"https://github.com/astrona-io/other"}]}`
	skipped := stopGuard(ctx, toolHookInput{SessionID: "chat-1", Cwd: dir}, logger)
	require.True(t, skipped != nil, "the skip is explained in the chat")
	assert.True(t, skipped["decision"] == nil, "verify_plan cannot answer for a checkout the server has not indexed, so nothing blocks")
	assert.Contains(t, skipped["systemMessage"].(string), "not in the server's index")
	assert.Contains(t, reported, `"repo":"acme/demo"`, "the name the checkout's origin gives")
	assert.Contains(t, reported, `"client":"claude-code"`)

	// A running job for the checkout: said so, and not counted as unindexed.
	reported = ""
	jobs = `{"jobs":[{"repo":"acme/demo","state":"running"}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Queued() {}\n"), 0o600))
	queued := stopGuard(ctx, toolHookInput{SessionID: "chat-3", Cwd: dir}, logger)
	require.True(t, queued != nil)
	assert.Contains(t, queued["systemMessage"].(string), "queued or being indexed")
	assert.Equal(t, reported, "")
	jobs = `{"jobs":[]}`

	// Same server, but the checkout is indexed: the check is asked for.
	reported = ""
	repos = `{"repos":[{"name":"` + filepath.Base(dir) + `","remote":""}]}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc ChangedAgain() {}\n"), 0o600))
	out := stopGuard(ctx, toolHookInput{SessionID: "chat-2", Cwd: dir}, logger)
	require.True(t, out != nil)
	assert.Contains(t, out["reason"].(string), "verify_plan")
	assert.Equal(t, reported, "", "an indexed checkout is not reported")
}

func TestHookCommandPrintsJSON(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	var out bytes.Buffer
	env := &app.Env{Stdin: strings.NewReader(`{"session_id":"chat-9","tool_input":{"file_path":"/nonexistent"}}`), Stdout: &out, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	require.NoError(t, (&hookOptions{EC: env, event: "read"}).Run(context.Background(), nil))
	assert.Equal(t, out.String(), "", "no opinion: no output, exit 0")

	out.Reset()
	env.Stdin = strings.NewReader(`not json`)
	require.NoError(t, (&hookOptions{EC: env, event: "stop"}).Run(context.Background(), nil))
	assert.Equal(t, out.String(), "", "unreadable input: no opinion")
	var none any
	assert.True(t, json.Unmarshal([]byte(`{}`), &none) == nil)
}

func TestLookupNotesHowTheSessionBegan(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	lookup := func(session, tool string) {
		noteLookup(toolHookInput{SessionID: session, ToolName: tool})
	}
	first := func(session string) string { return loadState(stateFile(session)).First }

	lookup("a", "mcp__plugin_codesynapse_codesynapse__minimal_context")
	lookup("a", "Grep")
	assert.Equal(t, first("a"), "codesynapse", "the first lookup decides")

	lookup("b", "Grep")
	lookup("b", "mcp__codesynapse__search")
	assert.Equal(t, first("b"), "native")

	lookup("c", "Bash")
	lookup("c", "mcp__other__search")
	assert.Equal(t, first("c"), "", "other tools are not code lookups")
}

func TestWorkflowReportOnlyTellsWhatIsNew(t *testing.T) {
	s := hookState{First: "native", Turned: 2}
	assert.Equal(t, s.workflowReport()["first_native"], 1)
	assert.Equal(t, s.workflowReport()["reads_turned_back"], 2)
	s.markReported()
	assert.True(t, s.workflowReport() == nil, "nothing new after a report")
	s.Turned = 5
	rep := s.workflowReport()
	assert.Equal(t, rep["reads_turned_back"], 3)
	assert.Equal(t, rep["first_native"], 0, "the beginning is reported once")
	assert.True(t, (hookState{}).workflowReport() == nil, "a session with no lookups reports nothing")
}

func TestSessionRulesFallBackToTheBuiltInText(t *testing.T) {
	t.Setenv("CODESYNAPSE_API_TOKEN", "csk_test")
	t.Setenv("CODESYNAPSE_API_URL", "http://127.0.0.1:1") // nothing listens
	out := sessionRules(context.Background(), toolHookInput{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	spec := out["hookSpecificOutput"].(map[string]any)
	assert.Equal(t, spec["hookEventName"], "SessionStart")
	assert.Contains(t, spec["additionalContext"].(string), "minimal_context")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.Header.Get("Authorization"), "Bearer csk_test")
		_, _ = io.WriteString(w, `{"version":0,"custom":true,"text":"Our rule: ask the graph first."}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODESYNAPSE_API_URL", srv.URL)
	out = sessionRules(context.Background(), toolHookInput{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.Equal(t, out["hookSpecificOutput"].(map[string]any)["additionalContext"], "Our rule: ask the graph first.")
}

// A checkout that was already dirty when the session began is not the
// session's change: the Stop hook asks only once the session changes it.
func TestStopGuardIgnoresWhatWasDirtyAtStart(t *testing.T) {
	offlineAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scratch.json"), []byte("{}"), 0o600))
	in := toolHookInput{SessionID: "chat-2", Cwd: dir}

	noteBaseline(ctx, in, logger)
	assert.True(t, stopGuard(ctx, in, logger) == nil, "an untracked file from before the session is not asked about")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "demo.go"), []byte("package demo\n\nfunc Changed() {}\n"), 0o600))
	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil, "a change made during the session is")
	assert.Equal(t, out["decision"], "block")

	// A session that began on a clean tree behaves as before.
	clean := gitRepo(t)
	in2 := toolHookInput{SessionID: "chat-3", Cwd: clean}
	noteBaseline(ctx, in2, logger)
	require.NoError(t, os.WriteFile(filepath.Join(clean, "demo.go"), []byte("package demo\n\nfunc Other() {}\n"), 0o600))
	assert.True(t, stopGuard(ctx, in2, logger) != nil)
}

// A session in a checkout starts with that repository's house style too.
func TestSessionRulesAddTheRepositoryConventions(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	t.Setenv("CODESYNAPSE_API_TOKEN", "csk_test")
	dir := gitRepo(t)
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/agent-rules":
			_, _ = io.WriteString(w, `{"text":"Ask the graph first."}`)
		case strings.HasSuffix(r.URL.Path, "/conventions"):
			asked = r.URL.EscapedPath()
			_, _ = io.WriteString(w, `{"conventions":["tests: assert with the repository's own helpers internal/testutil/assert (12 calls)","errors: fmt.Errorf (3 calls) and errors.New (1)"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("CODESYNAPSE_API_URL", srv.URL)
	out := sessionRules(context.Background(), toolHookInput{SessionID: "chat-c", Cwd: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text := out["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	assert.Equal(t, asked, "/api/v1/repos/acme%2Fdemo/conventions") // the name the server knows it by
	assert.Contains(t, text, "Ask the graph first.\n\nHouse style of acme/demo")
	assert.Contains(t, text, "\n- errors: fmt.Errorf (3 calls) and errors.New (1)")

	// Outside a checkout, or when the server has none, the rules stay as they are.
	out = sessionRules(context.Background(), toolHookInput{SessionID: "chat-d", Cwd: t.TempDir()}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	assert.Equal(t, out["hookSpecificOutput"].(map[string]any)["additionalContext"], "Ask the graph first.")
}

func TestStopGuardIgnoresWhatCodeSynapseDoesNotIndex(t *testing.T) {
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	in := toolHookInput{SessionID: "chat-1", Cwd: dir}
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".claude"), 0o755))
	for name, body := range map[string]string{".claude/settings.local.json": "{}", ".mcp.json": "{}", "notes.md": "# notes\n", "go.sum": "x\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	assert.True(t, stopGuard(ctx, in, logger) == nil, "local settings, JSON and prose are not asked about")
}

func TestStopGuardReportsGoProblemsItself(t *testing.T) {
	offlineAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "broken.go"), []byte("package demo\n\nfunc Broken( {\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ugly.go"), []byte("package demo\n\nfunc   Ugly()   {}\n"), 0o600))
	out := stopGuard(ctx, toolHookInput{SessionID: "chat-1", Cwd: dir}, logger)
	require.True(t, out != nil)
	reason := out["reason"].(string)
	assert.Contains(t, reason, "broken.go:3")
	assert.Contains(t, reason, "ugly.go: not gofmt-formatted")
	assert.Contains(t, reason, "verify_plan")
}

// TestStopGuardIgnoresUpstreamCommits: a checkout behind its upstream (others
// merged since it last pulled) has not changed the files those merges did.
func TestStopGuardIgnoresUpstreamCommits(t *testing.T) {
	offlineAPI(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	ctx := context.Background()
	dir := gitRepo(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	git := func(dir string, args ...string) {
		t.Helper()
		_, err := gitx.Run(ctx, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		require.NoError(t, err, strings.Join(args, " "))
	}
	// The upstream gains a commit merged elsewhere; this branch only tracks it.
	git(dir, "checkout", "-q", "-b", "upstream")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "merged.go"), []byte("package demo\n\nfunc Merged() {}\n"), 0o600))
	git(dir, "add", "-A")
	git(dir, "commit", "-q", "-m", "merged elsewhere")
	git(dir, "checkout", "-q", "main")
	git(dir, "branch", "-q", "--set-upstream-to=upstream")

	in := toolHookInput{SessionID: "chat-1", Cwd: dir}
	assert.True(t, stopGuard(ctx, in, logger) == nil, "upstream commits are not this session's change")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "local.go"), []byte("package demo\n\nfunc Local() {}\n"), 0o600))
	git(dir, "add", "-A")
	git(dir, "commit", "-q", "-m", "local")
	out := stopGuard(ctx, in, logger)
	require.True(t, out != nil)
	assert.Contains(t, out["reason"].(string), "1 file(s)", "only the unpushed commit counts")
}
