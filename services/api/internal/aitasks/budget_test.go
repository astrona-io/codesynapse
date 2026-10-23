package aitasks

import (
	"fmt"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func TestTaskRejectsOversizedFirstEvidence(t *testing.T) {
	for _, task := range []string{"security-triage", "review"} {
		t.Run(task, func(t *testing.T) {
			r, _, prompts := setup(t, `{"summary":"should not be called"}`, false)
			cfg, ok := r.LLM.Config("env")
			require.True(t, ok)
			lim := limitsFor(cfg)
			lim.chars = 1
			var err error
			if task == "security-triage" {
				_, err = r.securityTriage(taskContext(t), "env", Input{Repo: storetest.Repo}, sourceReader{}, lim)
			} else {
				_, err = r.codeReview(taskContext(t), "env", Input{Repo: storetest.Repo}, sourceReader{}, lim)
			}
			require.ErrorIs(t, err, llm.ErrInputBudget)
			assert.Len(t, *prompts, 0)
		})
	}
}

func TestTriagePreservesFindingsAcrossBudgetSplit(t *testing.T) {
	ctx := taskContext(t)
	r, st, prompts := setup(t, `{"summary":"Checked one.","items":[{"index":0,"verdict":"needs-review","explanation":"Checked."}]}`, false)
	g := storetest.Fixture()
	for i := range 3 {
		id := fmt.Sprintf("budget-finding-%d", i)
		g.AddNode(model.Node{ID: id, Kind: model.KindFinding, Repo: storetest.Repo, Name: id,
			Rule: "budget-rule", Severity: "critical", File: "app/app.go", StartLine: 11 + i,
			Summary: strings.Repeat("bounded detail ", 10)})
	}
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	rep, err := security.Build(ctx, st, storetest.Repo, security.Options{MinSeverity: "medium"})
	require.NoError(t, err)
	require.Len(t, rep.Findings, 3)
	cfg, ok := r.LLM.Config("env")
	require.True(t, ok)
	prompt, used, err := r.triagePrompt(ctx, "env", storetest.Repo, rep.Findings[:1], sourceReader{}, limitsFor(cfg))
	require.NoError(t, err)
	require.Len(t, used, 1)
	count, err := r.LLM.Preflight(ctx, "env", llm.Request{System: triageSystem, User: prompt})
	require.NoError(t, err)
	cfg.MaxInputTokens = count.Tokens
	r.LLM = llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cfg}, Default: "env"})
	rv, err := r.securityTriage(ctx, "env", Input{Repo: storetest.Repo}, sourceReader{}, limitsFor(cfg))
	require.NoError(t, err)
	require.Len(t, *prompts, 3)
	require.Len(t, rv.Comments, 3, "batch tails must be retried after a budget split")
	lines := map[int]bool{}
	for _, c := range rv.Comments {
		lines[c.Line] = true
	}
	assert.Len(t, lines, 3)
	assert.Equal(t, r.LLM.Usage("env").Requests, 3, "candidate preflight must not count as a model call")
}

func TestTaskLimitsBoundBytesWithoutOverflow(t *testing.T) {
	for _, n := range []int{-1, 0, 1, int(^uint(0) >> 1)} {
		lim := limitsFor(llm.ProviderConfig{MaxInputTokens: n})
		assert.True(t, lim.chars >= 0 && lim.chars <= llm.MaxPromptBytes)
	}
}

func TestBriefingKeepsOversizedDocumentOutOfPrompt(t *testing.T) {
	ctx := taskContext(t)
	r, st, prompts := setup(t, "A bounded briefing.", false)
	g := storetest.Fixture()
	g.AddNode(model.Node{ID: "oversized-document", Kind: model.KindDocument, Repo: storetest.Repo,
		Name: "Oversized", File: "large.md", Summary: strings.Repeat("OVERSIZED-DOCUMENT ", 1000)})
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	cfg, ok := r.LLM.Config("env")
	require.True(t, ok)
	lim := limitsFor(cfg)
	lim.chars = 1000
	_, err := r.briefing(ctx, "env", Input{Repo: storetest.Repo}, lim)
	require.NoError(t, err)
	require.Len(t, *prompts, 1)
	assert.True(t, len((*prompts)[0]) <= lim.chars)
	assert.NotContains(t, (*prompts)[0], "OVERSIZED-DOCUMENT")
}

func TestTasksPropagateUnavailableExactCount(t *testing.T) {
	for _, task := range []string{"security-triage", "review", "briefing"} {
		t.Run(task, func(t *testing.T) {
			r, _, prompts := setup(t, "should not be called", false)
			cfg, ok := r.LLM.Config("env")
			require.True(t, ok)
			cfg.InputBudgetPolicy = "require_exact"
			r.LLM = llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{cfg}, Default: "env"})
			_, err := r.Run(taskContext(t), task, Input{Repo: storetest.Repo})
			require.ErrorIs(t, err, llm.ErrExactCountUnavailable)
			assert.Len(t, *prompts, 0)
		})
	}
}
