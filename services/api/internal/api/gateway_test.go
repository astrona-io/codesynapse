package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/gateway"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func TestGatewayForwardsWithKeyAndBindsSession(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	key := strings.TrimPrefix(f.client.auth, "Bearer ")

	call := func(headers map[string]string) (int, string) {
		req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{"model":"m"}`)) //nolint:noctx // test
		require.NoError(t, err)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close() //nolint:errcheck // test
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}

	status, body := call(map[string]string{gateway.KeyHeader: key, "X-Api-Key": "sk-ant-own"})
	assert.Equal(t, status, 200)
	assert.Equal(t, body, `{"ok":true}`)
	assert.Equal(t, seen.Get("X-Api-Key"), "sk-ant-own")
	assert.Equal(t, seen.Get(gateway.KeyHeader), "")

	status, _ = call(map[string]string{"X-Api-Key": "sk-ant-own"})
	assert.Equal(t, status, 401, "no gateway key")
	status, _ = call(map[string]string{gateway.KeyHeader: "csk_nope", "X-Api-Key": "sk-ant-own"})
	assert.Equal(t, status, 401, "unknown gateway key")
	status, _ = call(map[string]string{gateway.KeyHeader: key})
	assert.Equal(t, status, 401, "no model credential")

	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "execution_session.register", Limit: 100})
	require.NoError(t, err)
	gw := 0
	for _, e := range rows {
		if strings.HasPrefix(e.Target, "gw:") {
			gw++
		}
	}
	assert.Equal(t, gw, 1, "one audited gateway session")
}

func TestGatewayAuditsCallsWithoutPrompts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	defer up.Close()
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"TOP-SECRET-PROMPT"}]}`)) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
	req.Header.Set("X-Api-Key", "sk-ant-own")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = res.Body.Close()
	require.Equal(t, res.StatusCode, 200)

	// The gateway audits once the handler returns, which can be after the
	// client has read the whole answer.
	var rows []accounts.Event
	for deadline := time.Now().Add(5 * time.Second); len(rows) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		rows, err = f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "gateway.request", Limit: 10})
		require.NoError(t, err)
	}
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0].Detail, "route=cloud")
	assert.Contains(t, rows[0].Detail, "model=claude-x")
	assert.NotContains(t, rows[0].Detail, "TOP-SECRET")
	assert.NotContains(t, rows[0].Detail, "sk-ant-own")
}

func TestGatewayCapsOpenRequestsPerKey(t *testing.T) {
	release := make(chan struct{})
	var held atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		held.Add(1)
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	defer up.Close()
	defer close(release)
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL})
	post := func() int {
		req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{}`)) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
		req.Header.Set("X-Api-Key", "sk-ant-own")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	for range maxGatewayInFlight {
		go post()
	}
	deadline := time.Now().Add(5 * time.Second)
	for held.Load() < maxGatewayInFlight && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, int(held.Load()), maxGatewayInFlight)
	assert.Equal(t, post(), http.StatusTooManyRequests)
}

func TestGatewayOffByDefault(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader("{}")) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set(gateway.KeyHeader, strings.TrimPrefix(f.client.auth, "Bearer "))
	req.Header.Set("X-Api-Key", "k")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	assert.Equal(t, res.StatusCode, 404)
}

func TestGatewayOpenAIBearerKeyAndNoLocalModel(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true})
	do := func(method, path, auth string) int {
		req, err := http.NewRequest(method, f.client.ts.URL+path, strings.NewReader(`{"model":"x","messages":[]}`)) //nolint:noctx // test
		require.NoError(t, err)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	assert.Equal(t, do(http.MethodGet, "/v1/models", ""), 401)
	assert.Equal(t, do(http.MethodGet, "/v1/models", "Bearer csk_nope"), 401)
	assert.Equal(t, do(http.MethodGet, "/v1/models", f.client.auth), 503, "valid key, no local model")
	assert.Equal(t, do(http.MethodPost, "/v1/chat/completions", f.client.auth), 503)
}

func TestGatewayOrganisationCanSwitchItOff(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true})
	status := func() int {
		req, err := http.NewRequest(http.MethodGet, f.client.ts.URL+"/v1/models", nil) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set("Authorization", f.client.auth)
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = res.Body.Close()
		return res.StatusCode
	}
	assert.Equal(t, status(), 503, "on by default, no local model")

	path := "/api/v1/accounts/orgs/" + f.org.Slug + "/gateway"
	var got struct {
		Enabled   bool `json:"enabled"`
		Available bool `json:"available"`
	}
	require.Equal(t, f.client.do("GET", path, "", &got), 200)
	assert.True(t, got.Enabled)
	assert.True(t, got.Available)
	assert.Equal(t, f.client.do("PUT", path, `{"enabled":false}`, nil), 403, "API keys cannot switch it off")
	assert.Equal(t, status(), 503)

	require.NoError(t, f.accounts.St.SetGatewayEnabled(t.Context(), f.org.ID, false))
	assert.Equal(t, status(), 403, "switched off for the organisation")
	require.Equal(t, f.client.do("GET", path, "", &got), 200)
	assert.False(t, got.Enabled)
}

func TestGatewayLocalFirstSetting(t *testing.T) {
	f := newRuntimeFixture(t, Config{Gateway: true})
	path := "/api/v1/accounts/orgs/" + f.org.Slug + "/gateway"
	type setting struct {
		Enabled        bool     `json:"enabled"`
		LocalFirst     bool     `json:"local_first"`
		LocalExclude   []string `json:"local_exclude"`
		ShortenPrompts bool     `json:"shorten_prompts"`
	}
	var got setting
	require.Equal(t, f.client.do("GET", path, "", &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true, LocalExclude: []string{}, ShortenPrompts: true}, "on by default, nothing excluded, shortening on")
	assert.Equal(t, f.client.do("PUT", path, `{"local_first":false}`, nil), 403, "API keys cannot change it")
	assert.Equal(t, f.client.do("PUT", path, `{"shorten_prompts":false}`, nil), 403, "API keys cannot turn shortening off")

	owner := newClient(t, f.client.ts)
	require.Equal(t, owner.do("POST", "/api/v1/auth/login", `{"email":"runtime@example.com","password":"`+pw+`"}`, nil), 200)
	assert.Equal(t, owner.do("PUT", path, `{}`, nil), 400, "nothing to change")
	require.Equal(t, owner.do("PUT", path, `{"local_first":false}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: false, LocalExclude: []string{}, ShortenPrompts: true}, "enabled is left alone")
	require.Equal(t, owner.do("PUT", path, `{"enabled":false}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: false, LocalFirst: false, LocalExclude: []string{}, ShortenPrompts: true}, "older clients send only enabled")
	require.Equal(t, owner.do("PUT", path, `{"enabled":true,"local_first":true}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true, LocalExclude: []string{}, ShortenPrompts: true})
	require.Equal(t, owner.do("PUT", path, `{"shorten_prompts":false}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true, LocalExclude: []string{}, ShortenPrompts: false})
	rows, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "org.gateway.shorten_prompts.set", Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the change is audited")
	assert.Equal(t, rows[0].Detail, "shorten_prompts=false")
	require.Equal(t, owner.do("PUT", path, `{"shorten_prompts":true}`, &got), 200)
	assert.True(t, got.ShortenPrompts)

	// The exclude list: validated before anything changes, normalized, and
	// left alone by PUTs that do not send it.
	for _, bad := range []string{
		`{"local_exclude":["claude opus"]}`, `{"local_exclude":["*"]}`, `{"local_exclude":["a*b"]}`,
		`{"local_exclude":["` + strings.Repeat("a", 65) + `"]}`, `{"local_exclude":"gpt-5"}`,
		`{"local_first":false,"local_exclude":["anthropic/claude"]}`,
	} {
		assert.Equal(t, owner.do("PUT", path, bad, nil), 400, bad)
	}
	var many []string
	for i := range 33 {
		many = append(many, `"m`+strings.Repeat("x", i+1)+`"`)
	}
	assert.Equal(t, owner.do("PUT", path, `{"local_exclude":[`+strings.Join(many, ",")+`]}`, nil), 400, "at most 32 entries")
	require.Equal(t, owner.do("GET", path, "", &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true, LocalExclude: []string{}, ShortenPrompts: true}, "a refused PUT changes nothing")
	assert.Equal(t, f.client.do("PUT", path, `{"local_exclude":["gpt-5"]}`, nil), 403, "API keys cannot change it")

	require.Equal(t, owner.do("PUT", path, `{"local_exclude":["GPT-5","claude-opus-*","gpt-5"]}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: true, LocalExclude: []string{"claude-opus-*", "gpt-5"}, ShortenPrompts: true})
	require.Equal(t, owner.do("PUT", path, `{"local_first":false}`, &got), 200)
	assert.Equal(t, got, setting{Enabled: true, LocalFirst: false, LocalExclude: []string{"claude-opus-*", "gpt-5"}, ShortenPrompts: true}, "the list is left alone")
	require.Equal(t, owner.do("PUT", path, `{"local_exclude":[]}`, &got), 200)
	assert.Equal(t, got.LocalExclude, []string{}, "an empty list clears it")
}

// TestGatewayExcludeRouting runs requests through the whole gateway: every
// model goes to the local model by default, and the organisation's and the
// installation's exclude lists send matching models to the cloud with the
// reason "excluded", also when local first is off.
func TestGatewayExcludeRouting(t *testing.T) {
	const answer = "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	loc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, answer) }))
	defer loc.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"cloud":true}`) }))
	defer up.Close()
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{
		Name: "local", Kind: llm.KindOpenAI, BaseURL: strings.Replace(loc.URL, "127.0.0.1", "localhost", 1) + "/v1", Model: "qwen",
		Gateway: true, GatewayExclude: []string{"gpt-5"},
	}}, Default: "local"})
	f := newRuntimeFixture(t, Config{Gateway: true, GatewayUpstream: up.URL, LLM: reg})
	key := strings.TrimPrefix(f.client.auth, "Bearer ")
	route := func(model string) string {
		req, err := http.NewRequest(http.MethodPost, f.client.ts.URL+"/v1/messages", strings.NewReader(`{"model":"`+model+`","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set(gateway.KeyHeader, key)
		req.Header.Set("X-Api-Key", "sk-ant-own")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		require.Equal(t, res.StatusCode, 200)
		if r := res.Header.Get(gateway.RouteHeader); r != "" {
			return r
		}
		return "cloud"
	}
	set := func(first bool, exclude ...string) {
		require.NoError(t, f.accounts.St.SetGatewayLocalFirst(t.Context(), f.org.ID, first))
		require.NoError(t, f.accounts.St.SetGatewayLocalExclude(t.Context(), f.org.ID, exclude))
	}
	cases := []struct {
		name    string
		first   bool
		exclude []string
		model   string
		want    string
	}{
		{"every model local by default", true, nil, "claude-opus-5-5", "local"},
		{"small model local by default", true, nil, "claude-haiku-5-5", "local"},
		{"excluded exact", true, []string{"claude-opus-5-5"}, "claude-opus-5-5", "cloud"},
		{"excluded prefix", true, []string{"claude-opus-*"}, "claude-opus-4-1", "cloud"},
		{"prefix leaves others local", true, []string{"claude-opus-*"}, "claude-sonnet-5-5", "local"},
		{"installation list", true, nil, "gpt-5", "cloud"},
		{"local first off keeps the small model", false, nil, "claude-haiku-5-5", "local"},
		{"exclusion wins with local first off", false, []string{"claude-haiku-*"}, "claude-haiku-5-5", "cloud"},
	}
	for _, tc := range cases {
		set(tc.first, tc.exclude...)
		assert.Equal(t, route(tc.model), tc.want, tc.name)
	}

	var rows []accounts.Event
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		var err error
		rows, err = f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "gateway.request", Limit: 20})
		require.NoError(t, err)
		if len(rows) == len(cases) {
			break
		}
	}
	excluded := 0
	for _, e := range rows {
		if strings.Contains(e.Detail, "reason=excluded") {
			excluded++
		}
	}
	assert.Equal(t, excluded, 4, "each excluded request is audited with its reason")
}

func TestRecordGatewayTimeAndFallthrough(t *testing.T) {
	ctx := telemetry.WithTenant(context.Background(), "abababababababababababab")
	reasons := []string{
		gateway.FallExcluded, gateway.FallNotEligible, gateway.FallTooLarge, gateway.FallHasThinking, gateway.FallHasImage,
		gateway.FallQueueTimeout, gateway.FallCancelled, gateway.FallTimeout, gateway.FallModelError,
		gateway.FallBadStatus, gateway.FallEmptyAnswer, gateway.FallTruncated, gateway.FallBadAnswer,
		gateway.FallNoLocalModel,
	}
	for _, reason := range reasons {
		recordGateway(ctx, gateway.Meta{Route: "cloud", Fallthrough: reason, Asked: true, QueueWait: time.Second, LocalAnswer: time.Second}, http.StatusOK, 2*time.Second)
	}
	recordGateway(ctx, gateway.Meta{Route: "local", Metered: true, Stream: true, Asked: true, LocalAnswer: 30 * time.Millisecond, FirstByte: 20 * time.Millisecond}, http.StatusOK, 40*time.Millisecond)
	// The OpenAI-compatible endpoint has no cloud behind it: its failures stay local.
	recordGateway(ctx, gateway.Meta{Route: "local", Fallthrough: gateway.FallBadStatus, Metered: true, InTokens: 999}, http.StatusBadGateway, time.Millisecond)
	recordGateway(ctx, gateway.Meta{Route: "sideways"}, http.StatusBadRequest, time.Millisecond)

	var out bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&out))
	text := out.String()
	const id = `tenant_id="abababababababababababab"`
	for _, reason := range reasons {
		assert.Contains(t, text, `gateway_fallthrough_total{reason="`+reason+`",route="cloud",`+id, "every fall-through point has its own reason")
		assert.Equal(t, telemetry.GatewayReason(reason), reason, "the telemetry allowlist mirrors the gateway's reasons")
	}
	assert.Contains(t, text, `gateway_fallthrough_total{reason="bad_status",route="local",`+id)
	assert.False(t, strings.Contains(text, `gateway_fallthrough_total{reason="other",`), "no gateway reason falls outside the fixed set")
	assert.Contains(t, text, `gateway_queue_wait_seconds_count{`+id)
	assert.Contains(t, text, `gateway_local_answer_seconds_count{outcome="failed",`+id)
	assert.Contains(t, text, `gateway_local_answer_seconds_count{outcome="answered",`+id)
	assert.Contains(t, text, `gateway_first_byte_seconds_count{route="local",`+id)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "gateway_local_tokens_total{"+id+`,kind="input"`) {
			assert.False(t, strings.HasSuffix(line, " 999"), "a failed local answer counts no tokens")
		}
	}
	assert.Contains(t, text, `gateway_request_duration_seconds_count{route="cloud",`+id)
	assert.Contains(t, text, `gateway_request_duration_seconds_count{route="local",`+id)
	assert.Contains(t, text, `gateway_request_duration_seconds_count{route="rejected",`+id)
}
