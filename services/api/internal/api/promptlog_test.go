package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
)

// promptLogBody is an agent turn: a system prompt, the user's words, a tool
// call and its result. Only the user's words may reach the prompt log.
const promptLogBody = `{"model":"claude-x","system":"SYSTEM-PROMPT","messages":[` +
	`{"role":"user","content":[{"type":"text","text":"=1+2 please fix it"}]},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"TOOL-OUTPUT"}]}]}`

func sendGateway(t *testing.T, f runtimeFixture) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(promptLogBody)) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
	req.Header.Set("X-Api-Key", "sk-ant-own")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, res.StatusCode, 200)
}

// gatewayEvents waits for the n-th gateway.request entry: the gateway
// audits once the handler returns, which can be after the client read the answer.
func gatewayEvents(t *testing.T, f runtimeFixture, n int) []accounts.Event {
	t.Helper()
	var rows []accounts.Event
	for deadline := time.Now().Add(5 * time.Second); len(rows) < n && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var err error
		rows, err = f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "gateway.request", Limit: 10})
		require.NoError(t, err)
	}
	require.Len(t, rows, n)
	return rows
}

func signIn(t *testing.T, ts *httptest.Server, email string) *client {
	t.Helper()
	c := newClient(t, ts)
	require.Equal(t, c.do("POST", "/api/v1/auth/login", `{"email":"`+email+`","password":"`+pw+`"}`, nil), 200)
	return c
}

func TestGatewayPromptLog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer up.Close()
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	owner := signIn(t, f.client.ts, "runtime@example.com")
	base := "/api/v1/accounts/orgs/" + f.org.Slug + "/gateway/prompt-log"
	sum := sha256.Sum256([]byte("=1+2 please fix it"))
	want := hex.EncodeToString(sum[:])

	// Off by default: the chain gets the digest, the log nothing.
	var set accounts.PromptLogSetting
	require.Equal(t, f.client.do("GET", base+"/settings", "", &set), 200, "members may see whether prompts are kept")
	assert.Equal(t, set, accounts.PromptLogSetting{RetentionDays: 30})
	sendGateway(t, f)
	events := gatewayEvents(t, f, 1)
	assert.Contains(t, events[0].Detail, "prompt_sha256="+want)
	assert.Contains(t, events[0].Detail, "shortened=false")
	for _, leak := range []string{"please fix", "SYSTEM-PROMPT", "TOOL-OUTPUT"} {
		assert.NotContains(t, events[0].Detail, leak, "no text in the chain")
	}
	rows, err := f.accounts.St.ListPromptLog(t.Context(), accounts.PromptLogFilter{OrgID: f.org.ID})
	require.NoError(t, err)
	assert.Len(t, rows, 0, "off by default writes nothing")

	// API keys can neither turn it on nor read or export it.
	assert.Equal(t, f.client.do("PUT", base+"/settings", `{"enabled":true}`, nil), 403)
	assert.Equal(t, f.client.do("GET", base, "", nil), 403)
	assert.Equal(t, f.client.do("GET", base+"/export?format=csv&since=2020-01-01T00:00:00Z", "", nil), 403)

	assert.Equal(t, owner.do("PUT", base+"/settings", `{"retention_days":0}`, nil), 400)
	require.Equal(t, owner.do("PUT", base+"/settings", `{"enabled":true,"retention_days":7}`, &set), 200)
	assert.Equal(t, set, accounts.PromptLogSetting{Enabled: true, RetentionDays: 7})
	sendGateway(t, f)
	events = gatewayEvents(t, f, 2)
	for deadline := time.Now().Add(5 * time.Second); len(rows) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rows, err = f.accounts.St.ListPromptLog(t.Context(), accounts.PromptLogFilter{OrgID: f.org.ID})
		require.NoError(t, err)
	}
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, row.AuditID, events[0].ID, "the row names its chain entry (newest first)")
	assert.Equal(t, row.PromptSHA256, want, "and carries the digest the chain holds")
	assert.Contains(t, events[0].Detail, "prompt_sha256="+row.PromptSHA256)
	assert.Equal(t, row.Prompt, "=1+2 please fix it")
	assert.Equal(t, row.Route, "cloud")
	assert.Equal(t, row.Answer, "", "only the local model's answers are kept")
	assert.True(t, row.SessionID != "", "the runtime session is recorded")

	// Reads and exports are audited and need the owner's signed-in session.
	var page struct {
		Entries []accounts.PromptLogEntry `json:"entries"`
	}
	require.Equal(t, owner.do("GET", base+"?route=cloud", "", &page), 200)
	require.Len(t, page.Entries, 1)
	assert.Equal(t, owner.do("GET", base+"?route=moon", "", nil), 400)
	var csvBody string
	require.Equal(t, owner.do("GET", base+"/export?format=csv&since=2020-01-01T00:00:00Z", "", &csvBody), 200)
	assert.Contains(t, csvBody, "'=1+2 please fix it", "formulas are neutralised")
	assert.Contains(t, csvBody, want)
	assert.Equal(t, owner.do("GET", base+"/export?format=csv", "", nil), 400, "a time range is required")
	var jsonl string
	require.Equal(t, owner.do("GET", base+"/export?format=jsonl&since=2020-01-01T00:00:00Z", "", &jsonl), 200)
	assert.Contains(t, jsonl, `"prompt":"=1+2 please fix it"`)
	for _, action := range []string{accounts.ActionPromptLogSet, accounts.ActionPromptLogRead, accounts.ActionPromptLogExport} {
		got, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: action, Limit: 10})
		require.NoError(t, err)
		assert.True(t, len(got) > 0, action+" is audited")
	}

	// Another organisation's admin gets 404, as for any organisation they cannot see.
	_, _, err = f.accounts.SignUp(t.Context(), "other@example.com", "Other", pw)
	require.NoError(t, err)
	other := signIn(t, f.client.ts, "other@example.com")
	assert.Equal(t, other.do("GET", base, "", nil), 404)
	assert.Equal(t, other.do("GET", base+"/export?format=jsonl&since=2020-01-01T00:00:00Z", "", nil), 404)
	assert.Equal(t, other.do("PUT", base+"/settings", `{"enabled":false}`, nil), 404)
	assert.Equal(t, other.do("GET", base+"/settings", "", nil), 404)
}
