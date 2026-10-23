package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

func TestFixRejectsStaleReviewBeforeQueue(t *testing.T) {
	ctx := t.Context()
	workspace := t.TempDir()
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r", Root: filepath.Join(workspace, "r"), Branch: "main", Head: strings.Repeat("a", 40)})
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	rv, err := reviews.Prepare(reviews.Review{Repo: "r", Title: "Old review", Commit: strings.Repeat("b", 40), Comments: []reviews.CommentItem{{File: "a.go", Line: 3, Message: "fix"}}}, time.Now())
	require.NoError(t, err)
	rv.Comments[0].Status = reviews.Accepted
	require.NoError(t, st.SaveReview(ctx, rv))
	cfg, err := llm.Load("", func(k string) string {
		if k == "CODESYNAPSE_LLM_BASE_URL" {
			return "http://localhost:9999/v1"
		}
		return ""
	})
	require.NoError(t, err)
	// No job runner or remote URL is supplied: rejection must precede both.
	s := &Server{cfg: Config{AllowPush: true, Workspace: workspace, LLM: llm.NewRegistry(cfg)}, st: st}
	r := httptest.NewRequest(http.MethodPost, "/fix", strings.NewReader(fmt.Sprintf(`{"comment_ids":[%q]}`, rv.Comments[0].ID)))
	r.SetPathValue("id", rv.ID)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleFixReview(w, r)
	assert.Equal(t, w.Code, http.StatusConflict)
	assert.Contains(t, w.Body.String(), "review revision differs")
	stored, err := st.GetReview(ctx, rv.ID)
	require.NoError(t, err)
	assert.True(t, stored.Comments[0].Fix == nil)
}
