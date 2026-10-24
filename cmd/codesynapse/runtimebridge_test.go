package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestRuntimeBridgeConcurrentChats(t *testing.T) {
	t.Setenv("CODESYNAPSE_RUNTIME_DIR", t.TempDir())
	for _, client := range []string{"codex", "cursor"} {
		for _, stateful := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stateful=%t", client, stateful), func(t *testing.T) {
				var mu sync.Mutex
				seen := map[string]string{}
				registered := map[string]bool{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, r.Header.Get("Authorization"), "Bearer csk_test")
					if r.URL.Path == "/api/v1/execution/sessions" {
						var in struct {
							Session string `json:"session_id"`
						}
						require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
						mu.Lock()
						registered[in.Session] = true
						mu.Unlock()
						_ = json.NewEncoder(w).Encode(execution.Identity{TenantID: "org", SubjectID: "user", SessionID: in.Session, ParentSessionID: in.Session})
						return
					}
					primary := r.Header.Get(execution.SessionHeader)
					assert.Equal(t, r.Header.Get(execution.ParentSessionHeader), primary)
					if r.Method == http.MethodDelete {
						return
					}
					var request struct {
						ID     json.RawMessage
						Method string
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					if request.Method == "initialize" {
						if stateful {
							w.Header().Set("Mcp-Session-Id", "transport-"+primary)
						}
						_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18"}}`, request.ID)
						return
					}
					if stateful {
						assert.Equal(t, r.Header.Get("Mcp-Session-Id"), "transport-"+primary)
					}
					if request.Method == "notifications/initialized" {
						w.WriteHeader(202)
						return
					}
					if request.Method == "tools/list" {
						_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, request.ID)
						return
					}
					if request.Method == "tools/call" {
						mu.Lock()
						assert.True(t, registered[primary])
						seen[string(request.ID)] = primary
						mu.Unlock()
						_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":%q}]}}`, request.ID, primary)
					}
				}))
				t.Cleanup(server.Close)
				c := apiClient{base: server.URL, token: "csk_test", http: server.Client()}
				var lines []string
				lines = append(lines, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
				for i := range 24 {
					primary := fmt.Sprintf("chat-%d", i%2)
					meta := map[string]any{"x-codex-turn-metadata": map[string]string{"thread_id": primary}}
					if client == "cursor" {
						call := fmt.Sprintf("call-%d-%t", i, stateful)
						require.NoError(t, writeRuntimeReceipt(c, "cursor", "call", call, primary))
						meta = map[string]any{"progressToken": call}
					}
					call, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i + 3, "method": "tools/call", "params": map[string]any{"name": "search", "arguments": map[string]string{"session_id": "forged-model-value"}, "_meta": meta}})
					require.NoError(t, err)
					lines = append(lines, string(call))
				}
				var output bytes.Buffer
				b := &bridge{url: server.URL, token: c.token, http: server.Client(), out: &output}
				b.routes = &runtimeRoutes{client: client, api: c, owner: b, chats: map[string]*runtimeChat{}}
				require.NoError(t, b.run(t.Context(), strings.NewReader(strings.Join(lines, "\n")+"\n")))
				require.Len(t, seen, 24)
				for i := range 24 {
					assert.Equal(t, seen[fmt.Sprint(i+3)], fmt.Sprintf("chat-%d", i%2))
				}
				for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
					assert.True(t, json.Valid([]byte(line)), "parallel replies remain JSONL")
				}
			})
		}
	}
}

func TestRuntimeMetadataDoesNotUseModelArguments(t *testing.T) {
	r := &runtimeRoutes{client: "codex"}
	for _, input := range []string{
		`{"params":{"arguments":{"session_id":"fake","_meta":{"threadId":"fake"}}}}`,
		`{"params":{"_meta":{"threadId":"first","x-codex-turn-metadata":{"thread_id":"second"}}}}`,
		`{"params":{"_meta":{"x-codex-turn-metadata":{"session_id":"ephemeral-run"}}}}`,
	} {
		_, err := r.identity([]byte(input))
		require.Error(t, err)
	}
	id, err := r.identity([]byte(`{"params":{"_meta":{"threadId":"real-thread","sessionId":"ephemeral-run"}}}`))
	require.NoError(t, err)
	assert.Equal(t, id, "real-thread")
}

func TestRuntimeReceiptsScopeAndSingleConsumption(t *testing.T) {
	t.Setenv("CODESYNAPSE_RUNTIME_DIR", t.TempDir())
	c := apiClient{base: "https://api.example", token: "csk_first"}
	require.NoError(t, writeRuntimeReceipt(c, "cursor", "call", "native-call", "chat-one"))
	require.Error(t, writeRuntimeReceipt(c, "cursor", "call", "native-call", "chat-two"))
	other := c
	other.token = "csk_second"
	_, err := readRuntimeReceipt(other, "cursor", "call", "native-call", true)
	require.Error(t, err)
	other = c
	other.base = "https://another.example"
	_, err = readRuntimeReceipt(other, "cursor", "call", "native-call", true)
	require.Error(t, err)
	id, err := readRuntimeReceipt(c, "cursor", "call", "native-call", true)
	require.NoError(t, err)
	assert.Equal(t, id, "chat-one")
	_, err = readRuntimeReceipt(c, "cursor", "call", "native-call", true)
	require.Error(t, err)
	require.NoError(t, writeRuntimeReceipt(c, "codex", "thread", "child-thread", "root-thread"))
	r := &runtimeRoutes{client: "codex", api: c}
	id, err = r.identity([]byte(`{"params":{"_meta":{"x-codex-turn-metadata":{"thread_id":"child-thread","parent_thread_id":"parent-thread"}}}}`))
	require.NoError(t, err)
	assert.Equal(t, id, "root-thread")
}
