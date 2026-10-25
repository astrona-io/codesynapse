package offload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory/extract"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// fakeModel answers chat completions with answer and records the prompt.
func fakeModel(t *testing.T, answer string, prompt *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if prompt != nil && len(req.Messages) > 1 {
			*prompt = req.Messages[1].Content
		}
		b, _ := json.Marshal(answer)
		_, _ = w.Write([]byte(`{"model":"qwen-local","choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func registry(url string) *llm.Registry {
	return llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "local", Kind: llm.KindOpenAI, BaseURL: url + "/v1", Model: "qwen", MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5},
		{Name: "cloud", Kind: llm.KindOpenAI, BaseURL: "https://api.example.com/v1", Model: "big", MaxOutputTokens: 4000, MaxInputTokens: 30000, TimeoutSeconds: 5},
	}, Default: "local"})
}

func waitDone(t *testing.T, w *Worker, id string) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := w.Get(id); j.State == Succeeded || j.State == Failed {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

func TestWorkerExtractsAndSaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	var prompt string
	srv := fakeModel(t, `{"triples":[
		{"subject":"me","predicate":"PREFERS","object":"Small PRs","confidence":0.9},
		{"subject":"Payments","predicate":"OWNED_BY","object":"Team Blue","confidence":0.95},
		{"subject":"x","predicate":"MAYBE","object":"y","confidence":0.2}]}`, &prompt)
	w := New(st, registry(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	go w.Run(ctx)

	j, err := submit(t, w, "repo", "", "claude-code", []extract.Turn{{Role: "user", Text: "I prefer small PRs. Payments is owned by Team Blue."}})
	require.NoError(t, err)
	assert.Equal(t, j.State, Queued)
	done := waitDone(t, w, j.ID)
	assert.Equal(t, done.State, Succeeded, done.Error)
	assert.Equal(t, done.Saved, 2)
	assert.Equal(t, done.Dropped, 1, "below the confidence threshold")
	assert.Equal(t, done.Provider, "local")
	assert.True(t, strings.Contains(prompt, "I prefer small PRs"))

	g, err := st.SearchMemory(ctx, "repo", "user", 10, false)
	require.NoError(t, err)
	require.Len(t, g.Facts, 1)
	assert.Equal(t, g.Facts[0].Source, "claude-code (extracted by qwen-local)")
	assert.Equal(t, g.Facts[0].Confidence, 0.9)

	// The next extraction is told which entities exist.
	j, err = submit(t, w, "repo", "", "claude-code", []extract.Turn{{Role: "user", Text: "anything"}})
	require.NoError(t, err)
	waitDone(t, w, j.ID)
	assert.True(t, strings.Contains(prompt, "- Payments"), "known entities are passed to the model")
	assert.Equal(t, len(w.List("repo")), 2)
}

func TestWorkerLocalOnly(t *testing.T) {
	reg := registry("http://127.0.0.1:1")
	require.NoError(t, reg.SetRoutes(llm.Routes{Default: "local", Tasks: map[string]string{Task: "cloud"}}))
	w := New(memory.New(), reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := submit(t, w, "repo", "", "a", []extract.Turn{{Role: "user", Text: "x"}})
	assert.True(t, errors.Is(err, ErrNoLocalModel), "a cloud provider is refused")
	_, err = submit(t, New(memory.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil))), "repo", "", "a", []extract.Turn{{Role: "user", Text: "x"}})
	assert.True(t, errors.Is(err, ErrNoLocalModel), "no providers at all")
}

func TestWorkerRechecksAccessBeforeSaving(t *testing.T) {
	for _, revokeAt := range []int{1, 2} {
		t.Run(fmt.Sprint(revokeAt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			st := memory.New()
			srv := fakeModel(t, `{"triples":[{"subject":"Payments","predicate":"OWNED_BY","object":"Team Blue","confidence":0.95}]}`, nil)
			w := New(st, registry(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
			checks := 0
			job, err := w.SubmitAuthorizedContext(workerContext(t, "tenant"), "repo", "tenant|repo", "agent", []extract.Turn{{Role: "user", Text: "Payments is owned by Team Blue."}}, func(context.Context) error {
				checks++
				if checks >= revokeAt {
					return errors.New("permission revoked")
				}
				return nil
			})
			require.NoError(t, err)
			go w.Run(ctx)
			done := waitDone(t, w, job.ID)
			assert.Equal(t, done.State, Failed)
			g, err := st.SearchMemory(ctx, "tenant|repo", "", 100, false)
			require.NoError(t, err)
			assert.Len(t, g.Facts, 0)
		})
	}
}

func TestWorkerFailuresAndLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := fakeModel(t, "Sorry, I cannot do that.", nil)
	w := New(memory.New(), registry(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := submit(t, w, "repo", "", "a", nil)
	require.Error(t, err)

	// Without a running worker the queue fills up instead of blocking.
	for range queueSize {
		_, err := submit(t, w, "repo", "", "a", []extract.Turn{{Role: "user", Text: "x"}})
		require.NoError(t, err)
	}
	_, err = submit(t, w, "repo", "", "a", []extract.Turn{{Role: "user", Text: "x"}})
	assert.True(t, errors.Is(err, ErrBusy))

	go w.Run(ctx)
	last := w.List("repo")[0]
	done := waitDone(t, w, last.ID)
	assert.Equal(t, done.State, Failed)
	assert.True(t, strings.Contains(done.Error, "no JSON object"), done.Error)
}

func TestWorkerPreservesAcceptedToolAfterRequestEnds(t *testing.T) {
	const tenant = "dddddddddddddddddddddddd"
	telemetry.AllowTools("offload_turns")
	ctx, cancel := context.WithCancel(telemetry.WithTool(telemetry.WithTenant(context.Background(), tenant), "offload_turns"))
	srv := fakeModel(t, `{"triples":[{"subject":"Payments","predicate":"OWNED_BY","object":"Team Blue","confidence":0.95}]}`, nil)
	w := New(memory.New(), registry(srv.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	checks := 0
	ctx, err := execution.With(ctx, execution.Identity{TenantID: tenant, SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"})
	require.NoError(t, err)
	job, err := w.SubmitAuthorizedContext(ctx, "repo", tenant+"|repo", "agent", []extract.Turn{{Role: "user", Text: "Payments is owned by Team Blue."}}, func(workerCtx context.Context) error {
		checks++
		assert.Equal(t, telemetry.Tenant(workerCtx), tenant)
		assert.Equal(t, telemetry.Tool(workerCtx), "offload_turns")
		return workerCtx.Err()
	})
	require.NoError(t, err)
	cancel()
	w.run(t.Context(), job.ID)
	done, ok := w.Get(job.ID)
	require.True(t, ok)
	assert.Equal(t, done.State, Succeeded, done.Error)
	assert.Equal(t, checks, 2)
}

func workerContext(t *testing.T, tenant string) context.Context {
	t.Helper()
	ctx, err := execution.With(t.Context(), execution.Identity{TenantID: tenant, SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"})
	require.NoError(t, err)
	return ctx
}
func submit(t *testing.T, w *Worker, repo, namespace, source string, turns []extract.Turn) (Job, error) {
	t.Helper()
	return w.SubmitAuthorizedContext(workerContext(t, "tenant"), repo, namespace, source, turns, func(context.Context) error { return nil })
}
func TestMissingWorkerIdentityIsRejected(t *testing.T) {
	w := New(memory.New(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := w.Submit("repo", "tenant|repo", "agent", []extract.Turn{{Role: "user", Text: "A owns B"}})
	assert.True(t, errors.Is(err, execution.ErrMissing))
	assert.Len(t, w.List("repo"), 0)
}

func TestShutdownDropsQueuedConversation(t *testing.T) {
	model := fakeModel(t, `{"triples":[]}`, nil)
	w := New(memory.New(), registry(model.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	job, err := submit(t, w, "repo", "tenant|repo", "runtime", []extract.Turn{{Role: "user", Text: "private conversation"}})
	require.NoError(t, err)
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	w.Run(stopped)
	w.mu.Lock()
	defer w.mu.Unlock()
	assert.Equal(t, w.jobs[job.ID].State, Failed)
	assert.Len(t, w.jobs[job.ID].turns, 0)
	assert.True(t, w.jobs[job.ID].authorize == nil)
}

// A long conversation must reach a model with a small input budget in several
// calls, none of them over the budget, instead of one the provider refuses.
func TestWorkerSplitsTurnsToTheInputBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var calls, longest int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		longest = max(longest, len(body))
		_, _ = w.Write([]byte(`{"model":"qwen-local","choices":[{"message":{"content":"{\"triples\":[{\"subject\":\"Payments\",\"predicate\":\"OWNED_BY\",\"object\":\"Team Blue\",\"confidence\":0.9}]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{
		{Name: "local", Kind: llm.KindOpenAI, BaseURL: srv.URL + "/v1", Model: "qwen", MaxOutputTokens: 1000, MaxInputTokens: 6000, TimeoutSeconds: 5},
	}, Default: "local"})
	w := New(memory.New(), reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go w.Run(ctx)

	var turns []extract.Turn
	for i := range 8 {
		turns = append(turns, extract.Turn{Role: "user", Text: fmt.Sprintf("turn %d: %s", i, strings.Repeat("word ", 600))})
	}
	j, err := submit(t, w, "repo", "", "claude-code", turns)
	require.NoError(t, err)
	done := waitDone(t, w, j.ID)
	assert.Equal(t, done.State, Succeeded, done.Error)
	assert.True(t, calls > 1, "the conversation is split into several model calls")
	assert.True(t, longest <= 6000+500, "each call stays near the budget")
	assert.True(t, done.Saved > 0)
}
