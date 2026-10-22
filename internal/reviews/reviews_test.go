package reviews

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

var now = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func TestPrepare(t *testing.T) {
	r, err := Prepare(Review{
		Repo: "r", Title: "  Review of auth  ", Author: "claude-code",
		Comments: []CommentItem{
			{File: "./internal/auth.go", Line: 10, Message: "Token logged: password=hunter2secret", Category: "security", Severity: "high", Suggestion: "log.Info(\"login\")"},
			{File: "web/app.vue", Message: "Missing alt text; the fixture holds card 4111 1111 1111 1111"},
		},
	}, now)
	require.NoError(t, err)
	assert.Equal(t, r.Title, "Review of auth")
	assert.Equal(t, r.Verdict, Comment, "default verdict")
	assert.NotEqual(t, r.ID, "")
	assert.Equal(t, r.Comments[0].File, "internal/auth.go")
	assert.NotContains(t, r.Comments[0].Message, "hunter2secret", "secrets redacted")
	assert.Contains(t, r.Comments[1].Message, "card [CARD]", "personal data masked")
	assert.Equal(t, r.Comments[1].Severity, "medium")
	assert.Equal(t, r.Comments[1].Category, "other")
	assert.Equal(t, r.Comments[0].Status, Open)
	c := r.Counts()
	assert.Equal(t, c.Open, 2)
	assert.Equal(t, c.BySeverity["high"], 1)
}

func TestPrepareRejects(t *testing.T) {
	tests := map[string]Review{
		"no title":       {Repo: "r"},
		"bad verdict":    {Title: "t", Verdict: "lgtm"},
		"bad severity":   {Title: "t", Comments: []CommentItem{{File: "a.go", Message: "m", Severity: "urgent"}}},
		"bad category":   {Title: "t", Comments: []CommentItem{{File: "a.go", Message: "m", Category: "vibes"}}},
		"path escape":    {Title: "t", Comments: []CommentItem{{File: "../etc/passwd", Message: "m"}}},
		"absolute path":  {Title: "t", Comments: []CommentItem{{File: "/etc/passwd", Message: "m"}}},
		"no message":     {Title: "t", Comments: []CommentItem{{File: "a.go"}}},
		"inverted range": {Title: "t", Comments: []CommentItem{{File: "a.go", Message: "m", Line: 9, EndLine: 3}}},
		"too many":       {Title: "t", Comments: make([]CommentItem, MaxComments+1)},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Prepare(r, now)
			assert.True(t, errors.Is(err, ErrInvalid), "got %v", err)
		})
	}
}

func TestLimits(t *testing.T) {
	r, err := Prepare(Review{Title: strings.Repeat("x", 500), Comments: []CommentItem{{File: "a.go", Message: strings.Repeat("m", 10000)}}}, now)
	require.NoError(t, err)
	assert.LessOrEqual(t, len([]rune(r.Title)), maxTitle)
	assert.LessOrEqual(t, len([]rune(r.Comments[0].Message)), maxMessage)
}

func TestApplyStatus(t *testing.T) {
	r, err := Prepare(Review{Title: "t", Comments: []CommentItem{{File: "a.go", Message: "m"}}}, now)
	require.NoError(t, err)
	id := r.Comments[0].ID
	require.NoError(t, ApplyStatus(&r, id, Resolved, "fixed in abc123", "codex", now.Add(time.Hour)))
	assert.Equal(t, r.Comments[0].Status, Resolved)
	assert.Equal(t, r.Counts().Resolved, 1)
	assert.Error(t, ApplyStatus(&r, id, "done", "", "", now))
	assert.Error(t, ApplyStatus(&r, "missing", Resolved, "", "", now))
}
