package llm

import (
	"context"
	"errors"
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
		{name: "negation dropped", text: prose + " Please never change the tests.", answer: short + " Change tests."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			local := &stubProvider{text: func(Request) string { return tc.answer }}
			r, cloud, ctx := compressRegistry(t, tc.on, local)
			got, err := r.ShortenProse(ctx, tc.text)
			if tc.ok {
				require.NoError(t, err)
				assert.Equal(t, got, short)
			} else {
				assert.True(t, errors.Is(err, ErrNoRewrite), "refusals are ErrNoRewrite")
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
	_, err := r.ShortenProse(t.Context(), strings.Repeat("Plain prose sentence number one hundred. ", 20))
	assert.True(t, errors.Is(err, ErrNoRewrite))
	assert.Equal(t, len(cloud.got), 0)
}

var shortenProse = strings.Repeat("Could you please look at the function `Load` because it really returns 42 entries instead of 40. ", 6)

// TestShortenProseYieldsTheLocalModel: shortening never waits for the local
// model's slot and never takes it from a request that waits for it.
func TestShortenProseYieldsTheLocalModel(t *testing.T) {
	local := &stubProvider{text: func(Request) string { return "`Load` returns 42 entries, not 40." }}
	r, _, ctx := compressRegistry(t, nil, local)

	held, err := r.slots["local"].acquire(ctx)
	require.NoError(t, err)
	_, err = r.ShortenProse(ctx, shortenProse)
	assert.True(t, errors.Is(err, ErrLocalBusy), "a running answer keeps the slot")

	waiting := make(chan func(), 1)
	go func() {
		release, _ := r.slots["local"].acquire(ctx)
		waiting <- release
	}()
	for _, w := r.slots["local"].counts(); w == 0; _, w = r.slots["local"].counts() {
		time.Sleep(time.Millisecond)
	}
	held()
	next := <-waiting
	_, err = r.ShortenProse(ctx, shortenProse)
	assert.True(t, errors.Is(err, ErrLocalBusy), "the waiting request went first")
	next()
	assert.Equal(t, len(local.got), 0, "the model was never asked while busy")

	got, err := r.ShortenProse(ctx, shortenProse)
	require.NoError(t, err)
	assert.Equal(t, got, "`Load` returns 42 entries, not 40.")
	assert.True(t, local.got[0].MaxTokens > 0 && local.got[0].MaxTokens < len(shortenProse), "the answer is capped")
	running, _ := r.slots["local"].counts()
	assert.Equal(t, running, 0, "the slot is free again")
}

// TestShortenProseSurvivesAPanic: the rewrite runs outside the request, where
// net/http does not recover panics. One in the provider must not end the
// process; the caller gets no rewrite and the slot comes free.
func TestShortenProseSurvivesAPanic(t *testing.T) {
	local := &stubProvider{text: func(Request) string { panic("provider broke") }}
	r, _, ctx := compressRegistry(t, nil, local)

	_, err := r.ShortenProse(ctx, shortenProse)
	assert.True(t, errors.Is(err, ErrNoRewrite), "a panic means no rewrite")
	for running, _ := r.slots["local"].counts(); running != 0; running, _ = r.slots["local"].counts() {
		time.Sleep(time.Millisecond)
	}
	release, ok := r.slots["local"].tryAcquire(ctx)
	require.True(t, ok, "the slot is free again")
	release()
}

// TestShortenProseIsNotCancelledMidAnswer: a caller that stops waiting does
// not hang up on the local model (Ollama logs that as HTTP 500); the answer
// runs to its end and holds the slot until then.
func TestShortenProseIsNotCancelledMidAnswer(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	hungUp := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-finish:
			hungUp <- false
		case <-r.Context().Done():
			hungUp <- true
			return
		}
		_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"short"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "local", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "m",
		MaxOutputTokens: 1000, MaxInputTokens: 100000, TimeoutSeconds: 30}}, Default: "local"})
	base, err := execution.With(t.Context(), execution.Identity{TenantID: "t", SubjectID: "u", SessionID: "s", ParentSessionID: "s"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(base)
	errs := make(chan error, 1)
	go func() {
		_, err := r.ShortenProse(ctx, shortenProse)
		errs <- err
	}()
	<-started
	cancel()
	assert.True(t, errors.Is(<-errs, context.Canceled), "the caller stops waiting at once")
	running, _ := r.slots["local"].counts()
	assert.Equal(t, running, 1, "the answer still holds the slot")
	_, err = r.ShortenProse(base, shortenProse)
	assert.True(t, errors.Is(err, ErrLocalBusy))
	close(finish)
	assert.False(t, <-hungUp, "the model's request was not cancelled")
	for running, _ := r.slots["local"].counts(); running != 0; running, _ = r.slots["local"].counts() {
		time.Sleep(time.Millisecond)
	}
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

// keepsFacts refuses a caveman rewrite that lost or changed anything a
// reader acts on, and accepts one that only dropped words.
func TestKeepsFacts(t *testing.T) {
	for name, tc := range map[string]struct {
		orig, short string
		want        bool
	}{
		"terse":            {"Could you please have a look at the `Load` function, it returns 42 entries.", "`Load` returns 42 entries.", true},
		"contraction":      {"Please do not touch the tests.", "Don't touch tests.", true},
		"negation dropped": {"Please do not touch the tests.", "Touch tests.", false},
		"never dropped":    {"We never push to main directly.", "Push to main directly.", false},
		"only dropped":     {"Change only the parser, nothing else.", "Change parser, nothing else.", false},
		"except dropped":   {"Run every test except the slow ones.", "Run every test, skip slow ones.", false},
		"number changed":   {"Set the limit to 40 requests.", "Limit 50 requests.", false},
		"path changed":     {"The bug is in internal/gitx/run.go somewhere.", "Bug in gitx/run.go.", false},
		"path kept":        {"The bug is in internal/gitx/run.go somewhere.", "Bug: internal/gitx/run.go.", true},
		"identifier lost":  {"The loadConfig helper returns an error.", "Helper returns error.", false},
		"snake case lost":  {"Read the max_tokens field first.", "Read tokens field first.", false},
		"quote changed":    {`The error says "file not found" every time.`, `Error: "not found" every time.`, false},
		"quote kept":       {`The error says "file not found" every time.`, `Error "file not found", every time.`, true},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, keepsFacts(tc.orig, tc.short), tc.want) })
	}
}
