package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestGatewayBench sends the synthetic Claude Code requests in
// testdata/gatewaybench through the gateway's own local path (translate, the
// call to the local model and the checks on its answer) and reports, per
// request, the route, the fall-through reason, the time to the model's first
// token, the total time and the tokens. It runs only when
// CODESYNAPSE_GATEWAY_BENCH_URL names a reachable OpenAI-compatible endpoint
// (`just gateway-bench`), so CI skips it. Results hold numbers and fixture
// names only, never prompt or answer text.
func TestGatewayBench(t *testing.T) {
	base := strings.TrimRight(os.Getenv("CODESYNAPSE_GATEWAY_BENCH_URL"), "/")
	if base == "" {
		t.Skip("set CODESYNAPSE_GATEWAY_BENCH_URL to a local OpenAI-compatible endpoint (see docs/operations/gateway.md)")
	}
	model := os.Getenv("CODESYNAPSE_GATEWAY_BENCH_MODEL")
	if model == "" {
		t.Skip("set CODESYNAPSE_GATEWAY_BENCH_MODEL to the local model's name")
	}
	key := os.Getenv("CODESYNAPSE_GATEWAY_BENCH_KEY")
	if err := reachable(base, key); err != nil {
		t.Skipf("local endpoint not reachable: %v", err)
	}
	window := benchInt(t, "CODESYNAPSE_GATEWAY_BENCH_CONTEXT", 0)
	runs := max(benchInt(t, "CODESYNAPSE_GATEWAY_BENCH_RUNS", 1), 1)
	timeout := time.Duration(benchInt(t, "CODESYNAPSE_GATEWAY_BENCH_TIMEOUT", 600)) * time.Second
	only := os.Getenv("CODESYNAPSE_GATEWAY_BENCH_ONLY")

	probe := &firstToken{}
	l := &Local{
		BaseURL: base, Model: model, APIKey: key, ContextWindow: window,
		Effort:          os.Getenv("CODESYNAPSE_GATEWAY_BENCH_EFFORT"),
		DisableThinking: os.Getenv("CODESYNAPSE_GATEWAY_BENCH_DISABLE_THINKING") == "true",
		Client: &http.Client{
			Transport:     probe,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	report := benchReport{Model: model, Effort: l.Effort, ContextWindow: window, Runs: runs}
	for run := 1; run <= runs; run++ {
		for _, f := range loadBenchFixtures(t) {
			if only != "" && !strings.Contains(f.Name, only) {
				continue
			}
			body := compactBody(t, f.Body)
			r := benchOne(l, probe, f.Name, run, body, timeout)
			// One line per request as it finishes: a slow model shows progress.
			t.Logf("%s run %d: %s %s, %d ms", r.Fixture, r.Run, r.Route, r.Reason, r.TotalMS)
			report.Results = append(report.Results, r)
		}
	}
	report.summarize()
	report.print(os.Stdout)
	if path := os.Getenv("CODESYNAPSE_GATEWAY_BENCH_OUT"); path != "" {
		b, err := json.MarshalIndent(report, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(b, '\n'), 0o644))
	}
}

// benchResult is one request; numbers and the fixture's name only.
type benchResult struct {
	Fixture      string `json:"fixture"`
	Run          int    `json:"run"`
	Route        string `json:"route"`            // local or cloud
	Reason       string `json:"reason,omitempty"` // the fall-through reason when the cloud would have answered
	RequestBytes int    `json:"request_bytes"`
	FirstTokenMS int64  `json:"first_token_ms"` // -1 when the model sent no token
	TotalMS      int64  `json:"total_ms"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type benchReport struct {
	Model         string         `json:"model"`
	Effort        string         `json:"effort,omitempty"`
	ContextWindow int            `json:"context_window"`
	Runs          int            `json:"runs"`
	Requests      int            `json:"requests"`
	Local         int            `json:"local"`
	LocalRate     float64        `json:"local_rate"`
	Reasons       map[string]int `json:"reasons"`
	Results       []benchResult  `json:"results"`
}

func benchOne(l *Local, probe *firstToken, name string, run int, body []byte, timeout time.Duration) benchResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	probe.reset()
	start := time.Now()
	w := httptest.NewRecorder()
	u, reason, ok := l.serve(ctx, w, body)
	total := time.Since(start)
	first, in, out := probe.result()
	r := benchResult{
		Fixture: name, Run: run, Route: "cloud", Reason: reason, RequestBytes: len(body),
		FirstTokenMS: -1, TotalMS: total.Milliseconds(), InputTokens: in, OutputTokens: out,
	}
	if ok {
		r.Route, r.Reason = "local", ""
		r.InputTokens, r.OutputTokens = max(u.In, in), max(u.Out, out)
	}
	if first > 0 {
		r.FirstTokenMS = first.Milliseconds()
	}
	return r
}

func (r *benchReport) summarize() {
	r.Requests, r.Local, r.Reasons = len(r.Results), 0, map[string]int{}
	for _, x := range r.Results {
		if x.Route == "local" {
			r.Local++
		} else {
			r.Reasons[x.Reason]++
		}
	}
	if r.Requests > 0 {
		r.LocalRate = float64(r.Local) / float64(r.Requests)
	}
}

func (r *benchReport) print(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	_, _ = fmt.Fprintln(tw, "fixture\trun\troute\treason\tbytes\tfirst token ms\ttotal ms\tin tok\tout tok\t")
	for _, x := range r.Results {
		reason := x.Reason
		if reason == "" {
			reason = "-"
		}
		first := "-"
		if x.FirstTokenMS >= 0 {
			first = strconv.FormatInt(x.FirstTokenMS, 10)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\t%s\t%d\t%d\t%d\t\n", x.Fixture, x.Run, x.Route, reason, x.RequestBytes, first, x.TotalMS, x.InputTokens, x.OutputTokens)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "\nmodel %s: %d of %d requests answered locally (%.0f%%)", r.Model, r.Local, r.Requests, 100*r.LocalRate)
	if len(r.Reasons) > 0 {
		var parts []string
		for _, reason := range []string{FallNotEligible, FallTooLarge, FallHasThinking, FallHasImage, FallExcluded, FallTimeout, FallCancelled, FallModelError, FallBadStatus, FallEmptyAnswer, FallTruncated, FallBadAnswer} {
			if n := r.Reasons[reason]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", reason, n))
			}
		}
		_, _ = fmt.Fprintf(w, "; to the cloud: %s", strings.Join(parts, ", "))
	}
	_, _ = fmt.Fprintln(w)
}

func reachable(base, key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := noRedirects.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /models answered %d", resp.StatusCode)
	}
	return nil
}

func benchInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	require.NoError(t, err, "%s must be a number", name)
	return n
}

// firstToken is the benchmark's transport to the local model: it notes when
// the first content, reasoning or tool-call delta arrives and the token counts
// the model reports, reading the stream as the gateway reads it. The serve
// call under test sees the same bytes.
type firstToken struct {
	mu      sync.Mutex
	sent    time.Time
	first   time.Duration
	in, out int
	done    chan struct{} // closed when watch has read the whole stream
}

func (p *firstToken) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent, p.first, p.in, p.out, p.done = time.Time{}, 0, 0, 0, nil
}

// result waits for the stream to be read: serve closes the body before it
// returns, so the watcher finishes promptly.
func (p *firstToken) result() (time.Duration, int, int) {
	p.mu.Lock()
	done := p.done
	p.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.first, p.in, p.out
}

func (p *firstToken) RoundTrip(req *http.Request) (*http.Response, error) {
	done := make(chan struct{})
	p.mu.Lock()
	p.sent, p.done = time.Now(), done
	p.mu.Unlock()
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	tee := io.TeeReader(resp.Body, pw)
	body := resp.Body
	go p.watch(pr, done)
	resp.Body = &teeBody{Reader: tee, close: func() error {
		_ = pw.Close()
		return body.Close()
	}, done: pw}
	return resp, nil
}

// teeBody closes the probe's pipe once the gateway has read the body to its
// end, so watch sees the whole stream.
type teeBody struct {
	io.Reader
	close func() error
	done  *io.PipeWriter
}

func (b *teeBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err != nil {
		_ = b.done.Close()
	}
	return n, err
}

func (b *teeBody) Close() error { return b.close() }

func (p *firstToken) watch(r io.Reader, done chan struct{}) {
	defer close(done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		data, found := bytes.CutPrefix(bytes.TrimSpace(sc.Bytes()), []byte("data:"))
		if !found {
			continue
		}
		var v struct {
			Choices []struct {
				Delta struct {
					Content          string            `json:"content"`
					Reasoning        string            `json:"reasoning"`
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &v) != nil {
			continue
		}
		p.mu.Lock()
		if p.first == 0 && len(v.Choices) > 0 {
			d := v.Choices[0].Delta
			if d.Content != "" || d.Reasoning != "" || d.ReasoningContent != "" || len(d.ToolCalls) > 0 {
				p.first = time.Since(p.sent)
			}
		}
		if v.Usage != nil {
			p.in, p.out = v.Usage.Prompt, v.Usage.Completion
		}
		p.mu.Unlock()
	}
	// Drain so the tee never blocks the gateway on a line too long to scan.
	_, _ = io.Copy(io.Discard, r)
}

// TestGatewayBenchHarness checks the measuring itself against a scripted
// model, so the opt-in benchmark cannot rot unnoticed.
func TestGatewayBenchHarness(t *testing.T) {
	cases := map[string]struct {
		answer, route, reason string
		out                   int
	}{
		"kept":      {completeAnswer, "local", "", 2},
		"truncated": {truncatedAnswer, "cloud", FallTruncated, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, c.answer)
			}))
			defer srv.Close()
			probe := &firstToken{}
			l := &Local{BaseURL: srv.URL, Model: "bench", Client: &http.Client{Transport: probe}}
			body := []byte(`{"model":"claude-opus-5-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
			r := benchOne(l, probe, "x", 1, body, 10*time.Second)
			require.Equal(t, r.Route, c.route)
			require.Equal(t, r.Reason, c.reason)
			require.Equal(t, r.OutputTokens, c.out)
			require.True(t, r.FirstTokenMS >= 0, "no first token measured")
			rep := benchReport{Results: []benchResult{r}}
			rep.summarize()
			var b strings.Builder
			rep.print(&b)
			require.Contains(t, b.String(), "requests answered locally")
		})
	}
}
