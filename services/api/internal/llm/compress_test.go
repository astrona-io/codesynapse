package llm

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type stubProvider struct {
	mu   sync.Mutex
	got  []Request
	text func(Request) string
}

func (s *stubProvider) Complete(_ context.Context, req Request) (Response, error) {
	s.mu.Lock()
	s.got = append(s.got, req)
	s.mu.Unlock()
	return Response{Text: s.text(req), Model: "m"}, nil
}
func (s *stubProvider) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func compressRegistry(t *testing.T, on *bool, local *stubProvider) (*Registry, *stubProvider, context.Context) {
	t.Helper()
	cloud := ProviderConfig{Name: "cloud", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000}
	loc := ProviderConfig{Name: "local", Kind: KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000}
	r := NewRegistry(Config{Providers: []ProviderConfig{cloud, loc}, Default: "cloud", CompressPrompts: on})
	cloudStub := &stubProvider{text: func(Request) string { return "ok" }}
	r.impl["cloud"], r.impl["local"] = cloudStub, local
	ctx, err := execution.With(t.Context(), execution.Identity{TenantID: "t", SubjectID: "u", SessionID: "s", ParentSessionID: "s"})
	require.NoError(t, err)
	return r, cloudStub, ctx
}

func TestCompressCloudShortensProse(t *testing.T) {
	prose := strings.Repeat("The function `Load` reads the configuration file and then, as a matter of fact, it basically returns 42 entries. ", 6)
	code := "```go\nfunc Load() {}\n```"
	local := &stubProvider{text: func(Request) string { return "`Load` read config, return 42 entries." }}
	r, cloud, ctx := compressRegistry(t, nil, local)
	_, err := r.Complete(ctx, "cloud", Request{System: "sys", User: prose + "\n\n" + code})
	require.NoError(t, err)
	require.Equal(t, len(cloud.got), 1)
	assert.True(t, strings.Contains(cloud.got[0].User, "`Load` read config"), "prose was rewritten")
	assert.True(t, strings.Contains(cloud.got[0].User, code), "code stays byte for byte")
	assert.True(t, strings.HasPrefix(cloud.got[0].System, "sys"), "system prompt prefix is unchanged")
	assert.True(t, r.Usage("cloud").CompressedBytes > 0)
	assert.Equal(t, local.got[0].System, compressSystem)
}

func TestCompressCloudKeepsOriginalWhenFactLost(t *testing.T) {
	prose := strings.Repeat("The function `Load` reads the configuration file and returns 42 entries to the caller. ", 6)
	local := &stubProvider{text: func(Request) string { return "reads config" }}
	r, cloud, ctx := compressRegistry(t, nil, local)
	_, err := r.Complete(ctx, "cloud", Request{System: "sys", User: prose})
	require.NoError(t, err)
	assert.Equal(t, cloud.got[0].User, prose, "a rewrite that lost `Load` and 42 is dropped")
}

func TestCompressCloudOffWhenDisabled(t *testing.T) {
	prose := strings.Repeat("Plain prose sentence number one hundred. ", 20)
	local := &stubProvider{text: func(Request) string { return "short" }}
	r, cloud, ctx := compressRegistry(t, new(false), local)
	_, err := r.Complete(ctx, "cloud", Request{System: "sys", User: prose})
	require.NoError(t, err)
	assert.Equal(t, len(local.got), 0)
	assert.Equal(t, cloud.got[0].System, "sys")
}

func TestCompressionStatus(t *testing.T) {
	cloud := ProviderConfig{Name: "cloud", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "m"}
	loc := ProviderConfig{Name: "local", Kind: KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "m"}
	for name, tc := range map[string]struct {
		cfg  Config
		want string
	}{
		"unset with a local model":        {Config{Providers: []ProviderConfig{cloud, loc}, Default: "cloud"}, CompressionOn},
		"only local models":               {Config{Providers: []ProviderConfig{loc}, Default: "local"}, CompressionNoCloudTask},
		"local default, cloud configured": {Config{Providers: []ProviderConfig{cloud, loc}, Default: "local"}, CompressionNoCloudTask},
		"local default, review to cloud":  {Config{Providers: []ProviderConfig{cloud, loc}, Default: "local", Tasks: map[string]string{"review": "cloud"}}, CompressionOn},
		"cloud default, every task local": {Config{Providers: []ProviderConfig{cloud, loc}, Default: "cloud", Tasks: map[string]string{
			"security-triage": "local", "review": "local", "briefing": "local", "fix": "local", "memory": "local", "code-annotation": "local",
		}}, CompressionNoCloudTask},
		"only embeddings to cloud":         {Config{Providers: []ProviderConfig{cloud, loc}, Default: "local", Tasks: map[string]string{"embeddings": "cloud"}}, CompressionNoCloudTask},
		"false with only local models":     {Config{Providers: []ProviderConfig{loc}, Default: "local", CompressPrompts: new(false)}, CompressionDisabled},
		"unset without a local model":      {Config{Providers: []ProviderConfig{cloud}, Default: "cloud"}, CompressionNoLocalModel},
		"true without a local model":       {Config{Providers: []ProviderConfig{cloud}, Default: "cloud", CompressPrompts: new(true)}, CompressionNoLocalModel},
		"compress routed to a cloud model": {Config{Providers: []ProviderConfig{cloud, loc}, Default: "cloud", Tasks: map[string]string{"compress": "cloud"}}, CompressionNoLocalModel},
		"false with a local model":         {Config{Providers: []ProviderConfig{cloud, loc}, Default: "cloud", CompressPrompts: new(false)}, CompressionDisabled},
		"no providers":                     {Config{}, CompressionNoLocalModel},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, NewRegistry(tc.cfg).CompressionStatus(), tc.want)
		})
	}
}

// Without a local model the default leaves cloud requests exactly as they
// were: no shortening, no terse-answer suffix.
func TestCompressCloudUntouchedWithoutLocalModel(t *testing.T) {
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "cloud", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000}}, Default: "cloud"})
	cloud := &stubProvider{text: func(Request) string { return "ok" }}
	r.impl["cloud"] = cloud
	ctx, err := execution.With(t.Context(), execution.Identity{TenantID: "t", SubjectID: "u", SessionID: "s", ParentSessionID: "s"})
	require.NoError(t, err)
	prose := strings.Repeat("Plain prose sentence number one hundred. ", 20)
	_, err = r.Complete(ctx, "cloud", Request{System: "sys", User: prose})
	require.NoError(t, err)
	require.Equal(t, len(cloud.got), 1, "only the answering call, no cloud call to shorten")
	assert.Equal(t, cloud.got[0].System, "sys")
	assert.Equal(t, cloud.got[0].User, prose)
}

func TestCompressible(t *testing.T) {
	long := strings.Repeat("word ", 100)
	for name, tc := range map[string]struct {
		in   string
		want bool
	}{
		"prose": {long, true}, "short": {"hi", false}, "fence": {long + "```x```", false},
		"diff": {long + "\n+added", false}, "json": {"{\"a\":1}\n" + long, false}, "list": {"- a\n" + long, false},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, compressible(tc.in), tc.want) })
	}
}

func TestShortenProse(t *testing.T) {
	prose := strings.Repeat("Could you please look at the function `Load` because it really returns 42 entries instead of 40. ", 6)
	short := "`Load` returns 42 entries, not 40."
	cases := []struct {
		name   string
		on     *bool
		text   string
		answer string
		ok     bool
	}{
		{name: "shortened", text: prose, answer: short, ok: true},
		{name: "fact lost", text: prose, answer: "Load is wrong."},
		{name: "not shorter", text: prose, answer: prose},
		{name: "turned off", on: new(false), text: prose, answer: short},
		{name: "code", text: prose + "\n```go\nfunc Load() {}\n```", answer: short},
		{name: "secret", text: prose + " The config has password=supersecret1 in it.", answer: short},
		{name: "too short", text: "fix `Load` please, 42", answer: "`Load` 42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := &stubProvider{text: func(Request) string { return tc.answer }}
			r, cloud, ctx := compressRegistry(t, tc.on, local)
			got, ok := r.ShortenProse(ctx, tc.text)
			assert.Equal(t, ok, tc.ok)
			if tc.ok {
				assert.Equal(t, got, short)
			} else {
				assert.Equal(t, got, "")
			}
			assert.Equal(t, len(cloud.got), 0, "the cloud model is never asked")
			if len(local.got) > 0 {
				assert.Equal(t, local.got[0].System, compressSystem)
			}
		})
	}
}

func TestShortenProseWithoutLocalModel(t *testing.T) {
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "cloud", Kind: KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000}}, Default: "cloud"})
	cloud := &stubProvider{text: func(Request) string { return "short" }}
	r.impl["cloud"] = cloud
	_, ok := r.ShortenProse(t.Context(), strings.Repeat("Plain prose sentence number one hundred. ", 20))
	assert.False(t, ok)
	assert.Equal(t, len(cloud.got), 0)
}

func TestRecordGatewayCompressed(t *testing.T) {
	r := NewRegistry(Config{Providers: []ProviderConfig{
		{Name: "claude", Kind: KindAnthropic, Model: "claude-opus-5-5", MaxOutputTokens: 100, MaxInputTokens: 100000},
		{Name: "local", Kind: KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000},
	}, Default: "claude"})
	r.RecordGatewayCompressed(t.Context(), "claude-opus-5-5", 120)
	assert.Equal(t, r.Usage("claude").CompressedBytes, 120, "a configured model gets the bytes")
	r.RecordGatewayCompressed(t.Context(), "claude-other", 80)
	r.RecordGatewayCompressed(t.Context(), "claude-opus-5-5", 0)
	assert.Equal(t, r.Usage("claude").CompressedBytes, 120, "another model goes to the gateway provider")
	assert.Equal(t, r.Usage(GatewayProvider), Usage{})
}

// The gateway shortens prompts bound for the cloud even when every task of
// CodeSynapse's own runs on the local model.
func TestCanShortenWithOnlyLocalTasks(t *testing.T) {
	local := ProviderConfig{Name: "local", Kind: KindOpenAI, BaseURL: "http://127.0.0.1:11434/v1", Model: "m", MaxOutputTokens: 100, MaxInputTokens: 100000}
	r := NewRegistry(Config{Providers: []ProviderConfig{local}, Default: "local"})
	assert.Equal(t, r.CompressionStatus(), CompressionNoCloudTask)
	assert.True(t, r.CanShorten())
	assert.False(t, NewRegistry(Config{Providers: []ProviderConfig{local}, Default: "local", CompressPrompts: new(false)}).CanShorten())
}
