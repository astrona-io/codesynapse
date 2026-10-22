package aitasks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
)

// fakeModel answers with canned JSON and records the prompts it saw.
func fakeModel(t *testing.T, answer string) (*httptest.Server, *[]string) {
	t.Helper()
	var prompts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompts = append(prompts, req.Messages[1].Content)
		resp := map[string]any{"model": "fake", "choices": []any{map[string]any{"message": map[string]string{"content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &prompts
}

func setup(t *testing.T, answer string, sendSource bool) (Runner, *memory.Store, *[]string) {
	t.Helper()
	ctx := taskContext(t)
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "app"), 0o750))
	var src strings.Builder
	for i := 1; i <= 40; i++ {
		src.WriteString("line\n")
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "app/app.go"), []byte(src.String()), 0o600))

	g := storetest.Fixture()
	repoNode, _ := g.Node(storetest.ID(model.KindRepository, storetest.Repo))
	repoNode.Root = root
	f := storetest.ID(model.KindFinding, "app/app.go:27:exec-shell")
	g.AddNode(model.Node{ID: f, Kind: model.KindFinding, Repo: storetest.Repo, Name: "Shell", Rule: "exec-shell", Severity: "critical", File: "app/app.go", StartLine: 27, EndLine: 27, Summary: "sh -c with input"})
	g.AddEdge(model.Edge{From: storetest.ID(model.KindMethod, "(*app.impl).Do"), To: f, Kind: model.EdgeHasFinding})
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))

	srv, prompts := fakeModel(t, answer)
	cfg, err := llm.Load("", func(k string) string {
		return map[string]string{"CODESYNAPSE_LLM_BASE_URL": srv.URL}[k]
	})
	require.NoError(t, err)
	cfg.Providers[0].SendSource = &sendSource
	return Runner{St: st, LLM: llm.NewRegistry(cfg)}, st, prompts
}

func TestSecurityTriage(t *testing.T) {
	answer := "Sure!\n```json\n{\"summary\":\"One real issue.\",\"items\":[{\"index\":0,\"verdict\":\"likely-exploitable\",\"explanation\":\"Input reaches sh -c.\",\"suggestion\":\"exec.Command(\\\"convert\\\", name)\"},{\"index\":7,\"verdict\":\"needs-review\",\"explanation\":\"ghost\"}]}\n```"
	r, st, prompts := setup(t, answer, true)
	id, err := r.Run(taskContext(t), "security-triage", Input{Repo: storetest.Repo})
	require.NoError(t, err)
	rv, err := st.GetReview(taskContext(t), id)
	require.NoError(t, err)
	assert.Equal(t, rv.Source, "ai")
	assert.Equal(t, rv.Verdict, reviews.RequestChanges)
	require.Len(t, rv.Comments, 1, "items for findings it was not given are dropped")
	c := rv.Comments[0]
	assert.Equal(t, c.File, "app/app.go")
	assert.Equal(t, c.Line, 27)
	assert.Equal(t, c.Rule, "exec-shell")
	assert.Contains(t, c.Message, "Likely exploitable")
	require.Len(t, *prompts, 1, "one request for all findings")
	assert.Contains(t, (*prompts)[0], "27: line", "snippet sent to a provider that allows source")
	assert.Contains(t, (*prompts)[0], "why it matters", "rule guide included")
}

func TestNoSourceForRemoteProviders(t *testing.T) {
	r, _, prompts := setup(t, `{"summary":"s","items":[]}`, false)
	_, err := r.Run(taskContext(t), "security-triage", Input{Repo: storetest.Repo})
	require.NoError(t, err)
	assert.NotContains(t, (*prompts)[0], "27: line")
}

func TestCodeReviewKeepsOnlyShownFiles(t *testing.T) {
	answer := `{"summary":"ok","verdict":"lgtm","comments":[{"file":"app/app.go","line":10,"severity":"HIGH","category":"bug","message":"nil check"},{"file":"elsewhere.go","line":1,"message":"invented"}]}`
	r, st, _ := setup(t, answer, true)
	id, err := r.Run(taskContext(t), "review", Input{Repo: storetest.Repo})
	require.NoError(t, err)
	rv, err := st.GetReview(taskContext(t), id)
	require.NoError(t, err)
	assert.Equal(t, rv.Verdict, reviews.Comment, "unknown verdict normalised")
	require.Len(t, rv.Comments, 1)
	assert.Equal(t, rv.Comments[0].Severity, "high")
}

func TestBriefingAndBadJSON(t *testing.T) {
	r, st, _ := setup(t, "## What this is\nA fixture.", true)
	id, err := r.Run(taskContext(t), "briefing", Input{Repo: storetest.Repo})
	require.NoError(t, err)
	rv, err := st.GetReview(taskContext(t), id)
	require.NoError(t, err)
	assert.Equal(t, rv.Kind, reviews.KindReport)
	assert.Contains(t, rv.Summary, "A fixture.")

	bad, _, _ := setup(t, "I cannot help with that.", true)
	_, err = bad.Run(taskContext(t), "security-triage", Input{Repo: storetest.Repo})
	assert.Error(t, err, "non-JSON answers fail clearly")
}

func TestLimitsScaleWithBudget(t *testing.T) {
	small := limitsFor(llm.ProviderConfig{MaxInputTokens: 12_000, MaxOutputTokens: 4000})
	assert.Equal(t, small.findings, 18)
	assert.Equal(t, small.batch, 6)
	assert.Equal(t, small.symbols, 10)
	assert.Equal(t, small.symbolLines, 80)

	large := limitsFor(llm.ProviderConfig{MaxInputTokens: 228_352, MaxOutputTokens: 32_768})
	assert.Equal(t, large.chars, 228_352*4)
	assert.Equal(t, large.findings, 152)
	assert.Equal(t, large.batch, 40)
	assert.Equal(t, large.symbols, 91)
	assert.Equal(t, large.symbolLines, 400)
	assert.Equal(t, large.padLines, 20)
}

func taskContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := execution.With(t.Context(), execution.Identity{TenantID: "tenant", SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"})
	require.NoError(t, err)
	return ctx
}

func TestRunRequiresExecutionBeforeStoreAccess(t *testing.T) {
	_, err := (Runner{}).Run(context.Background(), "briefing", Input{})
	require.ErrorIs(t, err, execution.ErrMissing)
}
