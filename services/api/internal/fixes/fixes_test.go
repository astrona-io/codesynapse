package fixes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
)

const original = "package app\n\nfunc Run(cmd string) {\n\texec(\"sh\", \"-c\", cmd)\n}\n"

func fakeModel(t *testing.T, answer string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "choices": []any{map[string]any{"message": map[string]string{"content": answer}, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type fixture struct {
	r      Runner
	st     *memory.Store
	req    Request
	pushed []string
	pr     map[string]any
}

func setup(ctx context.Context, t *testing.T, answer string, prStatus int) *fixture {
	t.Helper()
	repo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "app"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "app/run.go"), []byte(original), 0o600))
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "-m", "init"}} {
		_, err := gitx.Run(ctx, repo, args...)
		require.NoError(t, err)
	}
	st := memory.New()
	rv, err := reviews.Prepare(reviews.Review{Repo: "r", Title: "AI review", Comments: []reviews.CommentItem{
		{File: "app/run.go", Line: 4, Severity: "high", Category: "security", Message: "Shell injection. The command runs through sh -c.", Suggestion: "Call exec directly."},
		{File: ".github/workflows/ci.yml", Line: 1, Severity: "low", Category: "other", Message: "Pin the action."},
	}}, time.Now())
	require.NoError(t, err)
	for i := range rv.Comments {
		require.NoError(t, reviews.ApplyStatus(&rv, rv.Comments[i].ID, reviews.Accepted, "", "t", time.Now()))
	}
	require.NoError(t, st.SaveReview(ctx, rv))

	cfg, err := llm.Load("", func(k string) string {
		return map[string]string{"CODESYNAPSE_LLM_BASE_URL": fakeModel(t, answer) + "/v1"}[k]
	})
	require.NoError(t, err)
	f := &fixture{st: st}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/repos/owner/app/pulls")
		assert.Equal(t, r.Header.Get("Authorization"), "Bearer tok")
		_ = json.NewDecoder(r.Body).Decode(&f.pr)
		w.WriteHeader(prStatus)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/owner/app/pull/7","message":"nope"}`))
	}))
	t.Cleanup(gh.Close)
	f.r = Runner{St: st, LLM: llm.NewRegistry(cfg), Auth: gitx.Auth{Host: "github.com", Token: "tok"}, Author: gitx.Author{Name: "CodeSynapse", Email: "cs@localhost"},
		Workdir: t.TempDir(), GitHubAPI: gh.URL,
		Push: func(ctx context.Context, wt, _, branch string, _ gitx.Auth) error {
			out, err := gitx.Run(ctx, wt, "show", "--stat", "--format=%s", "HEAD")
			f.pushed = append(f.pushed, branch+"\n"+string(out))
			return err
		}}
	f.req = Request{ReviewID: rv.ID, CommentIDs: []string{rv.Comments[0].ID}, Root: repo, Remote: "https://github.com/owner/app",
		Base: gitx.HeadCommit(ctx, repo), BaseBranch: "main"}
	return f
}

const fixed = "Here you go:\n```go\npackage app\n\nfunc Run(cmd string) {\n\texec(cmd)\n}\n```\n"

func TestRunPushesBranchAndOpensDraftPR(t *testing.T) {
	ctx := context.Background()
	f := setup(ctx, t, fixed, http.StatusCreated)
	res, err := f.r.Run(ctx, f.req)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(res.Branch, "codesynapse/fix-"))
	assert.Equal(t, res.PRURL, "https://github.com/owner/app/pull/7")
	require.Len(t, f.pushed, 1)
	assert.Contains(t, f.pushed[0], "fix: Shell injection")
	assert.Contains(t, f.pushed[0], "app/run.go")
	assert.Equal(t, f.pr["draft"], true)
	assert.Equal(t, f.pr["base"], "main")

	rv, err := f.st.GetReview(ctx, f.req.ReviewID)
	require.NoError(t, err)
	require.NotNil(t, rv.Comments[0].Fix)
	assert.Equal(t, rv.Comments[0].Fix.State, reviews.FixPushed)
	assert.Equal(t, rv.Comments[0].Status, reviews.Accepted, "stays accepted until the fix is merged")
	// The indexed checkout is untouched.
	data, err := os.ReadFile(filepath.Join(f.req.Root, "app/run.go"))
	require.NoError(t, err)
	assert.Equal(t, string(data), original)
}

func TestRunKeepsBranchWhenPRFails(t *testing.T) {
	ctx := context.Background()
	f := setup(ctx, t, fixed, http.StatusForbidden)
	res, err := f.r.Run(ctx, f.req)
	require.NoError(t, err)
	assert.Equal(t, res.PRURL, "")
	assert.Contains(t, res.PRError, "HTTP 403")
	assert.Contains(t, res.CompareURL, "compare/main...codesynapse/fix-")
}

func TestRunRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("protected file", func(t *testing.T) {
		f := setup(ctx, t, fixed, http.StatusCreated)
		rv, _ := f.st.GetReview(ctx, f.req.ReviewID)
		f.req.CommentIDs = []string{rv.Comments[1].ID}
		_, err := f.r.Run(ctx, f.req)
		assert.Error(t, err)
		assert.Len(t, f.pushed, 0)
	})
	t.Run("not accepted", func(t *testing.T) {
		f := setup(ctx, t, fixed, http.StatusCreated)
		rv, _ := f.st.GetReview(ctx, f.req.ReviewID)
		require.NoError(t, reviews.ApplyStatus(&rv, rv.Comments[0].ID, reviews.Open, "", "t", time.Now()))
		require.NoError(t, f.st.SaveReview(ctx, rv))
		_, err := f.r.Run(ctx, f.req)
		assert.Error(t, err)
	})
	t.Run("no code block marks the fix failed", func(t *testing.T) {
		f := setup(ctx, t, "I would change exec.", http.StatusCreated)
		_, err := f.r.Run(ctx, f.req)
		assert.Error(t, err)
		assert.Len(t, f.pushed, 0)
		rv, _ := f.st.GetReview(ctx, f.req.ReviewID)
		require.NotNil(t, rv.Comments[0].Fix)
		assert.Equal(t, rv.Comments[0].Fix.State, reviews.FixFailed)
	})
}

func TestPlausible(t *testing.T) {
	big := strings.Repeat("x", 4000)
	assert.Error(t, plausible([]byte(big), []byte(big)))
	assert.Error(t, plausible([]byte(big), []byte("x")))
	assert.Error(t, plausible([]byte(big), []byte(" \n")))
	assert.NoError(t, plausible([]byte(big), []byte(big+"y")))
}

func TestProtected(t *testing.T) {
	for _, p := range []string{".github/workflows/ci.yml", "sub/.github/x", ".gitlab-ci.yml", "Jenkinsfile", ".git/config", ".gitmodules"} {
		assert.True(t, protected(p), p)
	}
	for _, p := range []string{"cmd/main.go", "docs/github.md", "Dockerfile"} {
		assert.False(t, protected(p), p)
	}
}

func TestRunAcceptsItsQueuedComments(t *testing.T) {
	ctx := context.Background()
	f := setup(ctx, t, fixed, http.StatusCreated)
	rv, _ := f.st.GetReview(ctx, f.req.ReviewID)
	reviews.SetFix(&rv, f.req.CommentIDs, reviews.Fix{State: reviews.FixQueued, Job: "j1"}, time.Now())
	require.NoError(t, f.st.SaveReview(ctx, rv))
	_, err := f.r.Run(ctx, f.req)
	require.NoError(t, err)
}

func TestSelectableStaleFix(t *testing.T) {
	c := reviews.CommentItem{File: "a.go", Status: reviews.Accepted}
	c.Fix = &reviews.Fix{State: reviews.FixRunning, At: time.Now().UTC().Format(time.RFC3339)}
	assert.NotEqual(t, Selectable(c), "", "a live fix blocks a second one")
	c.Fix.At = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	assert.Equal(t, Selectable(c), "", "an abandoned fix may be retried")
	c.Fix = &reviews.Fix{State: reviews.FixPushed, At: c.Fix.At}
	assert.NotEqual(t, Selectable(c), "")
}

func TestRunFailureBeforeStartIsRecorded(t *testing.T) {
	ctx := context.Background()
	f := setup(ctx, t, fixed, http.StatusCreated)
	f.req.Provider = "missing"
	_, err := f.r.Run(ctx, f.req)
	assert.Error(t, err)
	rv, _ := f.st.GetReview(ctx, f.req.ReviewID)
	require.NotNil(t, rv.Comments[0].Fix)
	assert.Equal(t, rv.Comments[0].Fix.State, reviews.FixFailed)
}
