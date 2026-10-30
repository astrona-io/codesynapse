package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeMCP is a streamable MCP endpoint that can be stopped (connections are
// dropped, as during a restart) and restarted (every session forgotten).
type fakeMCP struct {
	*httptest.Server
	up atomic.Bool
	// hang accepts requests and never answers, like a lost connection.
	hang atomic.Bool
	// runtimeGone refuses the runtime session until the API registers it
	// again; revoked refuses the key.
	runtimeGone, revoked atomic.Bool
	// notFound answers 404 to everything, like an ingress mid-upgrade.
	notFound atomic.Bool
	// breakCalls answers a tool call and then breaks the stream off.
	breakCalls atomic.Bool
	mu         sync.Mutex
	sessions   map[string]bool
	next       int
	methods    []string
}

func newFakeMCP(t *testing.T, up bool) *fakeMCP {
	t.Helper()
	f := &fakeMCP{sessions: map[string]bool{}}
	f.up.Store(up)
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func dropConnection(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	if !f.up.Load() {
		dropConnection(w)
		return
	}
	if f.hang.Load() {
		// The server notices a closed connection only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		return
	}
	if f.notFound.Load() {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	if f.revoked.Load() {
		http.Error(w, "log in or use a valid API key", http.StatusUnauthorized)
		return
	}
	if f.runtimeGone.Load() {
		http.Error(w, "runtime session is unavailable; register it with the API using the same personal key", http.StatusForbidden)
		return
	}
	if r.Header.Get("Authorization") != "Bearer csk_test" || r.Header.Get(execution.SessionHeader) != "chat-1" {
		http.Error(w, "missing runtime headers", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodDelete {
		return
	}
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	body, _ := io.ReadAll(r.Body)
	if json.Unmarshal(body, &m) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methods = append(f.methods, m.Method)
	if m.Method == "initialize" {
		f.next++
		id := fmt.Sprintf("mcp-%d", f.next)
		f.sessions[id] = true
		w.Header().Set("Mcp-Session-Id", id)
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"serverInfo":{"name":"codesynapse","version":"test"},"capabilities":{"tools":{}},"instructions":"Use the graph."}}`, m.ID, m.Params.ProtocolVersion)
		return
	}
	if !f.sessions[r.Header.Get("Mcp-Session-Id")] {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	switch m.Method {
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"minimal_context","inputSchema":{"type":"object","properties":{"repo":{"type":"string"}}}}]}}`, m.ID)
	case "tools/call":
		if f.breakCalls.Load() {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"answer\"}]}}\n\n", m.ID)
			w.(http.Flusher).Flush()
			dropConnection(w) // the chunked stream never ends properly
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"answer"}]}}`, m.ID)
	default:
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{}}`, m.ID)
	}
}

func (f *fakeMCP) restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = map[string]bool{}
}

func (f *fakeMCP) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

// bridgeAPI registers runtime sessions; mode is "up", "down" or "refuse".
type bridgeAPI struct {
	*httptest.Server
	mode  atomic.Value
	calls atomic.Int32
	// mcp, when set, learns of every registration (runtimeGone ends).
	mcp atomic.Pointer[fakeMCP]
}

func newBridgeAPI(t *testing.T, mode string) *bridgeAPI {
	t.Helper()
	a := &bridgeAPI{}
	a.mode.Store(mode)
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch a.mode.Load() {
		case "down":
			dropConnection(w)
			return
		case "refuse":
			a.calls.Add(1)
			http.Error(w, `{"detail":"invalid key"}`, http.StatusUnauthorized)
			return
		}
		a.calls.Add(1)
		if f := a.mcp.Load(); f != nil {
			f.runtimeGone.Store(false)
		}
		var in struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		_, _ = fmt.Fprintf(w, `{"tenant_id":"t","subject_id":"u","session_id":%q,"parent_session_id":%q}`, in.SessionID, in.SessionID)
	}))
	t.Cleanup(a.Close)
	return a
}

// chatOut hands each line the bridge writes to the test.
type chatOut struct{ lines chan []byte }

func (c chatOut) Write(p []byte) (int, error) {
	c.lines <- bytes.Clone(bytes.TrimSpace(p))
	return len(p), nil
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// chat drives one bridge as Claude Code would.
type chat struct {
	t    *testing.T
	b    *bridge
	in   *io.PipeWriter
	out  chatOut
	done chan error
	seen []reply
}

func fastProbes(t *testing.T) {
	t.Helper()
	wait, lo, hi, probe, live, alive := bridgeInitWait, probeMinDelay, probeMaxDelay, probeTimeout, callLiveWait, aliveTimeout
	bridgeInitWait, probeMinDelay, probeMaxDelay, probeTimeout = 300*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond, time.Second
	callLiveWait, aliveTimeout = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() {
		bridgeInitWait, probeMinDelay, probeMaxDelay, probeTimeout, callLiveWait, aliveTimeout = wait, lo, hi, probe, live, alive
	})
}

func isolateCache(t *testing.T) {
	t.Helper()
	dir, root := t.TempDir(), bridgeCacheRoot
	bridgeCacheRoot = func() (string, error) { return dir, nil }
	t.Cleanup(func() { bridgeCacheRoot = root })
}

func startChat(t *testing.T, mcpURL, apiURL, cacheDir string) *chat {
	t.Helper()
	out := chatOut{lines: make(chan []byte, 64)}
	b := &bridge{url: mcpURL, token: "csk_test", session: "chat-1", out: out, http: newBridgeHTTP(), cacheDir: cacheDir,
		report: apiClient{base: apiURL, token: "csk_test", http: &http.Client{Timeout: 2 * time.Second}}}
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	c := &chat{t: t, b: b, in: pw, out: out, done: make(chan error, 1)}
	go func() { c.done <- b.run(ctx, pr) }()
	t.Cleanup(func() {
		_ = pw.Close()
		select {
		case err := <-c.done:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("the bridge did not stop")
		}
		cancel()
		// Let a probe that is still running stop before the temp dirs go.
		b.probes.Wait()
	})
	return c
}

func (c *chat) send(line string) {
	c.t.Helper()
	_, err := io.WriteString(c.in, line+"\n")
	require.NoError(c.t, err)
}

// await returns the first line written that match accepts.
func (c *chat) await(what string, match func(reply) bool) reply {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line := <-c.out.lines:
			var r reply
			require.NoError(c.t, json.Unmarshal(line, &r), string(line))
			c.seen = append(c.seen, r)
			if match(r) {
				return r
			}
		case <-timeout:
			c.t.Fatalf("no %s within 5s", what)
		}
	}
}

func (c *chat) answer(id string) reply {
	c.t.Helper()
	return c.await("answer to "+id, func(r reply) bool { return string(r.ID) == id })
}

func (c *chat) call(id string) reply {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"minimal_context","arguments":{"question":"q"}}}`)
	return c.answer(id)
}

func (c *chat) initialize() reply {
	c.t.Helper()
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code"}}}`)
	r := c.answer("1")
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return r
}

func toolText(t *testing.T, r reply) (string, bool) {
	t.Helper()
	require.True(t, r.Error == nil, "a tool call gets a result, not a JSON-RPC error")
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	require.NoError(t, json.Unmarshal(r.Result, &res))
	require.Equal(t, len(res.Content), 1)
	return res.Content[0].Text, res.IsError
}

func toolNames(t *testing.T, r reply) []string {
	t.Helper()
	var res struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(r.Result, &res))
	names := []string{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func listChanged(r reply) bool { return r.Method == "notifications/tools/list_changed" }

func TestBridgeStartsWhileServerIsDown(t *testing.T) {
	fastProbes(t)
	t.Run("MCP server down", func(t *testing.T) {
		isolateCache(t)
		cache := t.TempDir()
		mcp, api := newFakeMCP(t, false), newBridgeAPI(t, "up")
		c := startChat(t, mcp.URL, api.URL, cache)

		init := c.initialize()
		require.True(t, init.Error == nil, "initialize is answered by the bridge")
		var res struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			Instructions string `json:"instructions"`
		}
		require.NoError(t, json.Unmarshal(init.Result, &res))
		assert.Equal(t, res.ProtocolVersion, "2025-06-18", "the client's version")
		assert.True(t, res.Capabilities.Tools.ListChanged, "the client listens for the tools once the server is back")
		assert.Contains(t, res.Instructions, "offline")

		c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		assert.Equal(t, len(toolNames(t, c.answer("2"))), 0, "nothing kept yet: no tools")
		started := time.Now()
		text, isErr := toolText(t, c.call("3"))
		assert.True(t, isErr)
		assert.Equal(t, text, offlineText)
		assert.True(t, time.Since(started) < 2*time.Second, "an offline call is answered at once")
		assert.True(t, codesynapseOffline("chat-1"), "the hooks are told")

		mcp.up.Store(true)
		c.await("list_changed", listChanged)
		assert.False(t, codesynapseOffline("chat-1"), "the marker goes once the server answers")
		c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
		assert.Equal(t, toolNames(t, c.answer("4")), []string{"minimal_context"})
		text, isErr = toolText(t, c.call("5"))
		assert.False(t, isErr)
		assert.Equal(t, text, "answer", "the same chat uses CodeSynapse again")
		assert.Equal(t, mcp.seen()[:2], []string{"initialize", "notifications/initialized"}, "the client's initialize was replayed")

		// The next chat that starts during an outage lists the kept tools.
		mcp.up.Store(false)
		next := startChat(t, mcp.URL, api.URL, cache)
		init = next.initialize()
		require.NoError(t, json.Unmarshal(init.Result, &res))
		assert.Contains(t, res.Instructions, "Use the graph.", "the kept instructions follow the offline line")
		next.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		assert.Equal(t, toolNames(t, next.answer("2")), []string{"minimal_context"})
		mcp.up.Store(true)
		next.await("list_changed", listChanged)
	})
	t.Run("API down: nothing is forwarded before the session is registered", func(t *testing.T) {
		isolateCache(t)
		mcp, api := newFakeMCP(t, true), newBridgeAPI(t, "down")
		c := startChat(t, mcp.URL, api.URL, t.TempDir())
		init := c.initialize()
		require.True(t, init.Error == nil, "initialize is answered after the registration wait")
		text, isErr := toolText(t, c.call("2"))
		assert.True(t, isErr)
		assert.Equal(t, text, offlineText)
		assert.Equal(t, len(mcp.seen()), 0, "an unregistered chat reaches nothing")

		api.mode.Store("up")
		c.await("list_changed", listChanged)
		text, isErr = toolText(t, c.call("3"))
		assert.False(t, isErr)
		assert.Equal(t, text, "answer")
	})
}

func TestBridgeReopensASessionAfterAServerRestart(t *testing.T) {
	fastProbes(t)
	isolateCache(t)
	mcp, api := newFakeMCP(t, true), newBridgeAPI(t, "up")
	c := startChat(t, mcp.URL, api.URL, t.TempDir())
	init := c.initialize()
	require.True(t, init.Error == nil)
	text, _ := toolText(t, c.call("2"))
	assert.Equal(t, text, "answer")

	mcp.restart()
	text, isErr := toolText(t, c.call("3"))
	assert.False(t, isErr, "a forgotten session is opened again and the call retried")
	assert.Equal(t, text, "answer")
	inits := 0
	for _, m := range mcp.seen() {
		if m == "initialize" {
			inits++
		}
	}
	assert.Equal(t, inits, 2)
	for _, r := range c.seen {
		assert.True(t, string(r.ID) != "1" || r.Result != nil && bytes.Equal(r.Result, init.Result), "the replayed initialize is not answered to the client")
	}
	assert.Equal(t, len(c.seen), 3, "one answer per request, nothing more")
}

func TestBridgeRefusedKeyForwardsNothing(t *testing.T) {
	fastProbes(t)
	isolateCache(t)
	mcp, api := newFakeMCP(t, true), newBridgeAPI(t, "refuse")
	c := startChat(t, mcp.URL, api.URL, t.TempDir())
	init := c.initialize()
	require.True(t, init.Error == nil, "the chat still connects")
	assert.Contains(t, string(init.Result), "refused")
	text, isErr := toolText(t, c.call("2"))
	assert.True(t, isErr)
	assert.Contains(t, text, "refused this chat")
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, len(mcp.seen()), 0, "nothing is forwarded")
	assert.Equal(t, api.calls.Load(), int32(1), "a refused key is not retried")
	assert.True(t, codesynapseOffline("chat-1"), "the hooks skip their guards")
}

func TestUnreachable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"header timeout", fmt.Errorf("%w after 30s", errHeaderTimeout), true},
		{"gateway", &mcpStatusError{Status: http.StatusBadGateway}, true},
		{"forgotten session", &mcpStatusError{Status: http.StatusNotFound}, false},
		{"refused", &mcpStatusError{Status: http.StatusForbidden}, false},
		{"API unavailable", &statusError{Status: http.StatusServiceUnavailable}, true},
		{"API refused", &statusError{Status: http.StatusUnauthorized}, false},
		{"other", io.ErrShortWrite, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, unreachable(tc.err), tc.want)
		})
	}
}

func TestHooksSkipGuardsWhileOffline(t *testing.T) {
	isolateCache(t)
	t.Setenv("CLAUDE_PLUGIN_DATA", t.TempDir())
	dir := gitRepo(t)
	big := filepath.Join(dir, "big.go")
	require.NoError(t, os.WriteFile(big, []byte("package big\n"+strings.Repeat("// line\n", 600)), 0o600))
	_, err := gitx.Run(context.Background(), dir, "add", "big.go")
	require.NoError(t, err)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	b := &bridge{session: "chat-1"}
	b.writeMarker(false)
	var in toolHookInput
	in.SessionID, in.ToolInput.FilePath, in.Cwd = "chat-1", big, dir
	assert.True(t, readGuard(context.Background(), in) == nil, "reads go through while offline")
	out := stopGuard(context.Background(), in, logger)
	require.True(t, out != nil)
	assert.True(t, out["decision"] == nil, "nothing blocks")
	assert.Equal(t, out["systemMessage"], stopOfflineNotice)

	b.clearMarker()
	assert.True(t, readGuard(context.Background(), in) != nil, "back online, the guard turns the read back")

	// No marker, but the API cannot be reached either: the stop check is
	// skipped, and asked again once the server is back.
	t.Setenv("CODESYNAPSE_API_URL", "http://127.0.0.1:1")
	t.Setenv(apiTokenEnv, "tok")
	out = stopGuard(context.Background(), in, logger)
	require.True(t, out != nil)
	assert.Equal(t, out["systemMessage"], stopOfflineNotice)
	refusingAPI(t)
	out = stopGuard(context.Background(), in, logger)
	require.True(t, out != nil)
	assert.Equal(t, out["decision"], "block", "the check runs on the next stop")
}

// callUntilAnswered repeats a tool call until the server answers it.
func (c *chat) callUntilAnswered(first int) string {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for id := first; ; id++ {
		text, isErr := toolText(c.t, c.call(fmt.Sprint(id)))
		if !isErr || time.Now().After(deadline) {
			return text
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestBridgeProbeAfterTheRuntimeSessionEnded(t *testing.T) {
	fastProbes(t)
	t.Run("the session is registered again", func(t *testing.T) {
		isolateCache(t)
		mcp, api := newFakeMCP(t, false), newBridgeAPI(t, "up")
		api.mcp.Store(mcp)
		c := startChat(t, mcp.URL, api.URL, t.TempDir())
		c.initialize()
		text, _ := toolText(t, c.call("2"))
		assert.Equal(t, text, offlineText)
		// The session expired during the outage: the replayed initialize is
		// refused until the API registers it again.
		mcp.runtimeGone.Store(true)
		mcp.up.Store(true)
		c.await("list_changed", listChanged)
		text, isErr := toolText(t, c.call("3"))
		assert.False(t, isErr)
		assert.Equal(t, text, "answer")
		assert.Equal(t, api.calls.Load(), int32(2), "registered at start and once more")
		assert.False(t, codesynapseOffline("chat-1"))
	})
	t.Run("a key revoked during the outage is a refusal", func(t *testing.T) {
		isolateCache(t)
		mcp, api := newFakeMCP(t, false), newBridgeAPI(t, "up")
		c := startChat(t, mcp.URL, api.URL, t.TempDir())
		c.initialize()
		mcp.revoked.Store(true)
		mcp.up.Store(true)
		deadline := time.Now().Add(5 * time.Second)
		for id := 2; ; id++ {
			text, isErr := toolText(t, c.call(fmt.Sprint(id)))
			assert.True(t, isErr)
			if text != offlineText {
				assert.Contains(t, text, "refused this chat")
				break
			}
			require.True(t, time.Now().Before(deadline), "the refusal was never noticed")
			time.Sleep(20 * time.Millisecond)
		}
		assert.True(t, codesynapseOffline("chat-1"), "the hooks skip their guards")
	})
	t.Run("another answer brings the chat online with the error", func(t *testing.T) {
		isolateCache(t)
		mcp, api := newFakeMCP(t, false), newBridgeAPI(t, "up")
		c := startChat(t, mcp.URL, api.URL, t.TempDir())
		c.initialize()
		mcp.notFound.Store(true)
		mcp.up.Store(true)
		c.await("list_changed", listChanged)
		r := c.call("2")
		// The server's error reaches the agent, as a JSON-RPC error or a tool error.
		text := ""
		if r.Error != nil {
			text = r.Error.Message
		} else {
			var isErr bool
			text, isErr = toolText(t, r)
			assert.True(t, isErr)
		}
		assert.True(t, text != offlineText, "a server that answers is not offline")
		assert.False(t, strings.Contains(text, "refused this chat"), "a 404 is not a refusal")
		assert.False(t, codesynapseOffline("chat-1"))
	})
}

func TestBridgeGivesUpOnALostConnection(t *testing.T) {
	fastProbes(t)
	isolateCache(t)
	mcp, api := newFakeMCP(t, true), newBridgeAPI(t, "up")
	c := startChat(t, mcp.URL, api.URL, t.TempDir())
	c.initialize()
	// Let the initialized notification through first: it has its own limit.
	for i := 0; i < 200 && !slices.Contains(mcp.seen(), "notifications/initialized"); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	mcp.hang.Store(true)
	started := time.Now()
	text, isErr := toolText(t, c.call("2"))
	assert.True(t, isErr)
	assert.Equal(t, text, offlineText)
	assert.True(t, time.Since(started) < 3*time.Second, "the call is not held for the whole limit")
	assert.True(t, codesynapseOffline("chat-1"), "the hooks are told")

	mcp.hang.Store(false)
	assert.Equal(t, c.callUntilAnswered(3), "answer", "the chat comes back")
}

func TestBridgeAnswersOnceWhenAStreamBreaksAfterTheAnswer(t *testing.T) {
	fastProbes(t)
	isolateCache(t)
	mcp, api := newFakeMCP(t, true), newBridgeAPI(t, "up")
	c := startChat(t, mcp.URL, api.URL, t.TempDir())
	c.initialize()
	mcp.breakCalls.Store(true)
	text, isErr := toolText(t, c.call("2"))
	assert.False(t, isErr)
	assert.Equal(t, text, "answer")
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	c.answer("3")
	answers := 0
	for _, r := range c.seen {
		if string(r.ID) == "2" {
			answers++
		}
	}
	assert.Equal(t, answers, 1, "one answer per id")
	assert.False(t, codesynapseOffline("chat-1"), "the server answered: not offline")
}

func TestRefusedForGood(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"API 401", &statusError{Status: http.StatusUnauthorized}, true},
		{"API 403", &statusError{Status: http.StatusForbidden}, true},
		{"API 404 from an ingress", &statusError{Status: http.StatusNotFound}, false},
		{"API 400", &statusError{Status: http.StatusBadRequest}, false},
		{"API 500", &statusError{Status: http.StatusInternalServerError}, false},
		{"MCP 401", &mcpStatusError{Status: http.StatusUnauthorized}, true},
		{"MCP 404", &mcpStatusError{Status: http.StatusNotFound}, false},
		{"identity conflict", execution.ErrConflict, true},
		{"network", io.ErrUnexpectedEOF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, refusedForGood(tc.err), tc.want)
		})
	}
}

func TestOfflineMarker(t *testing.T) {
	isolateCache(t)
	for _, session := range []string{".", ".."} {
		if file := offlineFile(session); file != "" {
			assert.Equal(t, filepath.Base(file), "session-"+session, "a marker never names the folder or its parent")
		}
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"just now", time.Now(), true},
		{"old", time.Now().Add(-offlineFresh - time.Minute), false},
		{"far in the future", time.Now().Add(24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(offlineMarker{At: tc.at})
			require.NoError(t, err)
			writePrivate(offlineFile("chat-1"), data)
			assert.Equal(t, codesynapseOffline("chat-1"), tc.want)
		})
	}
}
