package llm

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestLocalProviderRunsOneRequestAtATime(t *testing.T) {
	var now, peak atomic.Int32
	local := &stubProvider{text: func(Request) string {
		n := now.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		now.Add(-1)
		return "ok"
	}}
	r, _, ctx := compressRegistry(t, new(false), local)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Complete(ctx, "local", Request{System: "s", User: "u"})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.Equal(t, int(peak.Load()), 1)
	assert.Equal(t, len(local.got), 5, "every queued request is answered")
}

func TestQueuedRequestStopsWhenContextEnds(t *testing.T) {
	s := newSlots(ProviderConfig{BaseURL: "http://127.0.0.1:1/v1"})
	release, err := s.acquire(context.Background())
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = s.acquire(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.True(t, newSlots(ProviderConfig{BaseURL: "https://api.example.com/v1"}) == nil, "cloud is unlimited by default")
}

func TestQueueRecordsWaitCancelAndTimeout(t *testing.T) {
	p := ProviderConfig{Name: "queue-metrics", Kind: "openai", BaseURL: "http://127.0.0.1:1/v1", Model: "qm"}
	telemetry.AllowModels(p.Name, p.Model)
	s := newSlots(p)
	const tenant = "efefefefefefefefefefefef"
	ctx := WithTask(telemetry.WithTenant(context.Background(), tenant), "review")
	release, err := s.acquire(ctx)
	require.NoError(t, err)

	waited := make(chan error, 1)
	go func() {
		r, err := s.acquire(ctx)
		if err == nil {
			r()
		}
		waited <- err
	}()
	deadline, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	_, err = s.acquire(deadline)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	cancelled, cancel := context.WithCancel(WithTask(ctx, gatewayTask))
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, err = s.acquire(cancelled)
	assert.ErrorIs(t, err, context.Canceled)
	release()
	release() // a second call must not free another request's slot
	require.NoError(t, <-waited)
	running, waiting := s.counts()
	assert.Equal(t, running, 0)
	assert.Equal(t, waiting, 0)

	var out bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&out))
	text := out.String()
	id := `tenant_id="` + tenant + `",mcp_tool="none",llm_provider="queue-metrics",llm_model="qm"`
	assert.Contains(t, text, `llm_queue_wait_seconds_count{task="review",outcome="acquired",`+id+`} 2`)
	assert.Contains(t, text, `llm_queue_wait_seconds_bucket{task="review",outcome="acquired",`+id+`,le="0.005"} 1`)
	assert.Contains(t, text, `llm_queue_wait_seconds_count{task="review",outcome="timeout",`+id+`} 1`)
	assert.Contains(t, text, `llm_queue_wait_seconds_count{task="gateway",outcome="cancelled",`+id+`} 1`)
	assert.Contains(t, text, `llm_queue_running{`+id+`} 0`)
	assert.Contains(t, text, `llm_queue_waiting{`+id+`} 0`)
	assert.Contains(t, text, `platform_llm_queue_capacity{llm_provider="queue-metrics"} 1`)
}

func TestAnswerStartNamesItsBasis(t *testing.T) {
	cfg := ProviderConfig{Name: "start-metrics", Model: "sm"}
	telemetry.AllowModels(cfg.Name, cfg.Model)
	ctx := WithTask(telemetry.WithTenant(context.Background(), "abcabcabcabcabcabcabcabc"), "briefing")
	record(ctx, cfg.Name, cfg, Response{Duration: 3 * time.Second, FirstToken: time.Second}, nil)
	record(ctx, cfg.Name, cfg, Response{Duration: 5 * time.Second}, nil)
	record(ctx, cfg.Name, cfg, Response{Duration: 7 * time.Second}, errors.New("refused"))
	var out bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&out))
	id := `tenant_id="abcabcabcabcabcabcabcabc",mcp_tool="none",llm_provider="start-metrics",llm_model="sm"`
	assert.Contains(t, out.String(), `llm_answer_start_seconds_sum{task="briefing",basis="first_token",`+id+`} 1`)
	assert.Contains(t, out.String(), `llm_answer_start_seconds_sum{task="briefing",basis="full_answer",`+id+`} 5`, "a failed request never started answering")
}

func TestGatewayTargetNeedsOptInAndLocal(t *testing.T) {
	local := ProviderConfig{Name: "l", Kind: "openai", BaseURL: "http://localhost:11434/v1", Model: "m", Gateway: true}
	cloud := ProviderConfig{Name: "c", Kind: "openai", BaseURL: "https://api.example.com/v1", Model: "m", Gateway: true}
	r := NewRegistry(Config{Providers: []ProviderConfig{cloud}})
	_, ok := r.GatewayTarget()
	assert.True(t, !ok, "a cloud provider is never a gateway target")
	local.Gateway = false
	r = NewRegistry(Config{Providers: []ProviderConfig{local}})
	_, ok = r.GatewayTarget()
	assert.True(t, !ok, "off unless the provider opts in")
	local.Gateway = true
	r = NewRegistry(Config{Providers: []ProviderConfig{local}})
	got, ok := r.GatewayTarget()
	assert.True(t, ok)
	assert.Equal(t, got.Model, "m")
}

func TestGatewayExcludeAndWait(t *testing.T) {
	base := ProviderConfig{Name: "l", Kind: "openai", BaseURL: "http://localhost:11434/v1", Model: "m", Gateway: true}
	cases := map[string]struct {
		exclude []string
		wait    int
		want    []string
		dur     time.Duration
		bad     bool
	}{
		"defaults":          {nil, 0, []string{}, DefaultGatewayWait, false},
		"normalized":        {[]string{"Claude-Opus-*", "gpt-5", "gpt-5"}, 0, []string{"claude-opus-*", "gpt-5"}, DefaultGatewayWait, false},
		"wait set":          {nil, 5, []string{}, 5 * time.Second, false},
		"longest wait":      {nil, 300, []string{}, 300 * time.Second, false},
		"wait too long":     {nil, 301, nil, 0, true},
		"negative wait":     {nil, -1, nil, 0, true},
		"bad pattern":       {[]string{"claude opus"}, 0, nil, 0, true},
		"star in the midst": {[]string{"claude-*-5"}, 0, nil, 0, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			p.GatewayExclude, p.GatewayWaitSeconds = tc.exclude, tc.wait
			cfg := Config{Providers: []ProviderConfig{p}}
			err := cfg.normalise(func(string) string { return "" })
			if tc.bad {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			got, ok := NewRegistry(cfg).GatewayTarget()
			require.True(t, ok)
			assert.Equal(t, got.Exclude, tc.want)
			assert.Equal(t, got.Wait, tc.dur)
		})
	}
}
