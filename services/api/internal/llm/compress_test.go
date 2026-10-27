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

func compressRegistry(t *testing.T, on bool, local *stubProvider) (*Registry, *stubProvider, context.Context) {
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
	r, cloud, ctx := compressRegistry(t, true, local)
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
	r, cloud, ctx := compressRegistry(t, true, local)
	_, err := r.Complete(ctx, "cloud", Request{System: "sys", User: prose})
	require.NoError(t, err)
	assert.Equal(t, cloud.got[0].User, prose, "a rewrite that lost `Load` and 42 is dropped")
}

func TestCompressCloudOffByDefault(t *testing.T) {
	prose := strings.Repeat("Plain prose sentence number one hundred. ", 20)
	local := &stubProvider{text: func(Request) string { return "short" }}
	r, cloud, ctx := compressRegistry(t, false, local)
	_, err := r.Complete(ctx, "cloud", Request{System: "sys", User: prose})
	require.NoError(t, err)
	assert.Equal(t, len(local.got), 0)
	assert.Equal(t, cloud.got[0].System, "sys")
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
