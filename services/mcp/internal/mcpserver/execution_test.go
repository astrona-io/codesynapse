package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type runtimeTransport struct {
	mu                               sync.Mutex
	token, primary, parent, protocol string
}

func (r *runtimeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+r.token)
	clone.Header.Set(execution.SessionHeader, r.primary)
	clone.Header.Set(execution.ParentSessionHeader, r.parent)
	if id := clone.Header.Get("Mcp-Session-Id"); id != "" {
		r.protocol = id
	}
	r.mu.Unlock()
	return http.DefaultTransport.RoundTrip(clone)
}

func (r *runtimeTransport) metadata(primary, parent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primary, r.parent = primary, parent
}

func runtimeProbe(t *testing.T, acc *accounts.Service) (string, *atomic.Int64) {
	t.Helper()
	access := AccountsAccess{Acc: acc}
	runtime := NewRuntime(access, "", "", "")
	s := New(memory.New(), "test", Options{Access: access, Runtime: runtime})
	calls := &atomic.Int64{}
	mcp.AddTool(s, &mcp.Tool{Name: "rule_guide"}, func(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, execution.Identity, error) {
		id, err := execution.Require(ctx)
		calls.Add(1)
		return nil, id, err
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	ts := httptest.NewServer(auth.RequireBearerToken(KeyVerifier(acc, ""), &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(runtime.HTTP(handler)))
	t.Cleanup(ts.Close)
	return ts.URL, calls
}

func runtimeConnect(t *testing.T, url string, tr *runtimeTransport) *mcp.ClientSession {
	t.Helper()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "runtime-client", Version: "1"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: tr}}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func runtimeDenied(t *testing.T, cs *mcp.ClientSession) {
	t.Helper()
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "rule_guide", Arguments: map[string]any{}})
	assert.True(t, err != nil || result != nil && result.IsError, "rejected before tool execution")
}

func TestRuntimeHTTPBindingAndRevocation(t *testing.T) {
	_, keys, acc, _, _ := keyServer(t)
	p, err := acc.AuthenticateKey(t.Context(), keys["a"])
	require.NoError(t, err)
	_, err = acc.RegisterExecutionSession(t.Context(), p.User.ID, p.OrgID, "chat-second")
	require.NoError(t, err)
	url, calls := runtimeProbe(t, acc)
	tr := &runtimeTransport{token: keys["a"], primary: "chat-a", parent: "chat-a"}
	cs := runtimeConnect(t, url, tr)
	var id execution.Identity
	require.False(t, call(t, cs, "rule_guide", map[string]any{}, &id).IsError)
	assert.Equal(t, id.SessionID, "chat-a")
	assert.Equal(t, id.TenantID, p.OrgID)
	for _, tc := range []struct{ name, primary, parent string }{
		{"missing", "", ""}, {"missing-parent", "chat-a", ""}, {"forged-parent", "chat-a", "chat-second"},
		{"switch-registered", "chat-second", "chat-second"}, {"cross-tenant", "chat-b", "chat-b"},
		{"unregistered", "guessed-chat", "guessed-chat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := &runtimeTransport{token: keys["a"], primary: "chat-a", parent: "chat-a"}
			connection := runtimeConnect(t, url, attempt)
			before := calls.Load()
			attempt.metadata(tc.primary, tc.parent)
			runtimeDenied(t, connection)
			assert.Equal(t, calls.Load(), before)
		})
	}
	tr.metadata("chat-a", "chat-a")
	require.False(t, call(t, cs, "rule_guide", map[string]any{}, nil).IsError)
	tr.mu.Lock()
	protocol := tr.protocol
	tr.mu.Unlock()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
		require.NoError(t, err)
		req.Header.Set("Mcp-Session-Id", protocol)
		req.Header.Set("Authorization", "Bearer "+keys["a"])
		req.Header.Set(execution.SessionHeader, "chat-second")
		req.Header.Set(execution.ParentSessionHeader, "chat-second")
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, response.StatusCode, http.StatusForbidden, "resumed streams cannot switch primary context")
		require.NoError(t, response.Body.Close())
	}
	// A new transport may resume the same primary chat, or start another
	// already-registered chat. Neither transport is itself the primary ID.
	reconnected := runtimeConnect(t, url, &runtimeTransport{token: keys["a"], primary: "chat-a", parent: "chat-a"})
	require.False(t, call(t, reconnected, "rule_guide", map[string]any{}, &id).IsError)
	assert.Equal(t, id.SessionID, "chat-a")
	second := runtimeConnect(t, url, &runtimeTransport{token: keys["a"], primary: "chat-second", parent: "chat-second"})
	require.False(t, call(t, second, "rule_guide", map[string]any{}, &id).IsError)
	assert.Equal(t, id.SessionID, "chat-second")
	require.NoError(t, acc.RevokeExecutionSession(t.Context(), p.User.ID, p.OrgID, "chat-a"))
	runtimeDenied(t, cs)
	runtimeDenied(t, reconnected)
	require.False(t, call(t, second, "rule_guide", map[string]any{}, &id).IsError)
}

func TestRuntimeConcurrentSessions(t *testing.T) {
	_, keys, acc, _, _ := keyServer(t)
	url, calls := runtimeProbe(t, acc)
	clients := map[string]*mcp.ClientSession{}
	for _, name := range []string{"a", "b", "c"} {
		clients[name] = runtimeConnect(t, url, &runtimeTransport{token: keys[name], primary: "chat-" + name, parent: "chat-" + name})
	}
	var wg sync.WaitGroup
	for name, cs := range clients {
		wg.Go(func() {
			for range 10 {
				var id execution.Identity
				result := call(t, cs, "rule_guide", map[string]any{}, &id)
				assert.False(t, result.IsError)
				assert.Equal(t, id.SessionID, "chat-"+name)
				assert.Equal(t, id.ParentSessionID, id.SessionID)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, calls.Load(), int64(30))
}

func TestRuntimeMissingCannotUseToolArguments(t *testing.T) {
	s := New(memory.New(), "test")
	ct, st := mcp.NewInMemoryTransports()
	_, err := s.Connect(t.Context(), st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "missing-runtime"}, nil).Connect(t.Context(), ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "repo_list", Arguments: map[string]any{"session_id": "forged", "parent_session_id": "forged"}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
