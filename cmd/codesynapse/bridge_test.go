package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestBridgeForwardsWithRuntimeHeaders(t *testing.T) {
	var mu sync.Mutex
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer csk_test" || r.Header.Get(execution.SessionHeader) != "chat-1" || r.Header.Get(execution.ParentSessionHeader) != "chat-1" {
			http.Error(w, "missing runtime headers", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleted = r.Header.Get("Mcp-Session-Id") == "mcp-1"
			mu.Unlock()
			return
		}
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &m))
		if m.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "mcp-1" || r.Header.Get("Mcp-Protocol-Version") != "2025-06-18") {
			http.Error(w, "no transport session", http.StatusBadRequest)
			return
		}
		switch m.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "mcp-1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, m.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":%s,\"result\":{\"tools\":[]}}\n\n", m.ID)
		default:
			http.Error(w, "refused", http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	b := &bridge{url: srv.URL, token: "csk_test", session: "chat-1", out: &out, http: srv.Client()}
	require.NoError(t, b.run(context.Background(), strings.NewReader(in)))

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct{ Message string }
		}
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		if m.Error != nil {
			got[string(m.ID)] = "error: " + m.Error.Message
		} else {
			got[string(m.ID)] = string(m.Result)
		}
	}
	assert.Equal(t, got["1"], `{"protocolVersion":"2025-06-18"}`)
	assert.Equal(t, got["2"], `{"tools":[]}`, "server-sent events are unwrapped, multi-line data joined")
	assert.True(t, strings.HasPrefix(got["3"], "error: CodeSynapse answered 403"), "a refused request still gets an answer: "+got["3"])
	assert.Equal(t, len(got), 3, "notifications get no answer")
	mu.Lock()
	defer mu.Unlock()
	assert.True(t, deleted, "the transport session is closed at the end")
}

func TestCheckBridgeURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"http://localhost:8090/mcp":      true,
		"http://127.0.0.1:8090/mcp":      true,
		"http://[::1]:8090/mcp":          true,
		"https://cs.example.com/mcp":     true,
		"http://cs.example.com/mcp":      false,
		"https://u:p@cs.example.com/mcp": false,
		"ftp://localhost/mcp":            false,
		"localhost:8090":                 false,
	} {
		t.Run(raw, func(t *testing.T) {
			assert.Equal(t, checkBridgeURL(raw) == nil, ok)
		})
	}
}

func TestBrewStablePath(t *testing.T) {
	for bin, want := range map[string]string{
		"/opt/homebrew/Cellar/codesynapse/0.2.0/bin/codesynapse":              "/opt/homebrew/opt/codesynapse/bin/codesynapse",
		"/home/linuxbrew/.linuxbrew/Cellar/codesynapse/0.2.0/bin/codesynapse": "/home/linuxbrew/.linuxbrew/opt/codesynapse/bin/codesynapse",
		"/opt/homebrew/bin/codesynapse":                                       "/opt/homebrew/bin/codesynapse",
		"/Users/me/go/bin/codesynapse":                                        "/Users/me/go/bin/codesynapse",
	} {
		t.Run(bin, func(t *testing.T) {
			assert.Equal(t, brewStablePath(bin), want)
		})
	}
}

func TestWithToolsets(t *testing.T) {
	for _, tc := range []struct {
		url  string
		sel  []string
		want string
	}{
		{"http://localhost:8090/mcp", nil, "http://localhost:8090/mcp"},
		{"http://localhost:8090/mcp", []string{"core"}, "http://localhost:8090/mcp?toolsets=core"},
		{"https://cs.example.com/mcp?x=1", []string{"core", "memory"}, "https://cs.example.com/mcp?toolsets=core%2Cmemory&x=1"},
	} {
		got, err := withToolsets(tc.url, tc.sel)
		require.NoError(t, err)
		assert.Equal(t, got, tc.want, tc.url)
	}
}

// Claude Code probes stdio servers with server/discover before initialize.
// The bridge used to wait for initialize inside its read loop, so it never
// read the initialize behind the probe and the client timed out.
func TestBridgeAnswersAProbeBeforeInitialize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &m))
		assert.Equal(t, m.Method, "initialize", "the probe never reaches the server")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, m.ID)
	}))
	t.Cleanup(srv.Close)

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	b := &bridge{url: srv.URL, token: "csk_test", session: "chat-1", out: &out, http: srv.Client()}
	done := make(chan error, 1)
	go func() { done <- b.run(context.Background(), strings.NewReader(in)) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the bridge deadlocked on a request that came before initialize")
	}

	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m struct {
			ID    json.RawMessage `json:"id"`
			Error *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		if m.Error != nil {
			got[string(m.ID)] = fmt.Sprintf("error %d", m.Error.Code)
		} else {
			got[string(m.ID)] = "result"
		}
	}
	assert.Equal(t, got["0"], "error -32601", "a probe is told the method is unknown")
	assert.Equal(t, got["1"], "result", "initialize is still answered")
}
