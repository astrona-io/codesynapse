package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x"}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

func TestReviewCode(t *testing.T) {
	ctx := context.Background()
	sources := t.TempDir()
	root := filepath.Join(sources, "proj")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "app"), 0o750))
	var src strings.Builder
	for i := 1; i <= 20; i++ {
		src.WriteString("line " + string(rune('a'+i-1)) + "\n")
	}
	src.WriteString(`password = "hunter2hunter2"` + "\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "app", "a.go"), []byte(src.String()), 0o600))
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "add", ".")
	gitT(t, root, "commit", "-q", "-m", "one")
	commit := gitT(t, root, "rev-parse", "HEAD")
	// The working tree moves on after the review; the code shown must not.
	require.NoError(t, os.WriteFile(filepath.Join(root, "app", "a.go"), []byte("changed\n"), 0o600))

	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("proj", model.KindRepository, "proj"), Kind: model.KindRepository, Repo: "proj", Name: "proj",
		Root: root, URL: "https://github.com/acme/proj.git", Head: commit, Branch: "main"})
	require.NoError(t, st.ReplaceRepo(ctx, "proj", g))
	rv, err := reviews.Prepare(reviews.Review{Repo: "proj", Title: "T", Commit: commit, Comments: []reviews.CommentItem{
		{File: "app/a.go", Line: 10, EndLine: 11, Message: "m"},
		{File: "app/a.go", Line: 21, Message: "secret"},
		{File: "app/missing.go", Line: 1, Message: "gone"},
		{File: "app/a.go", Message: "no line"},
	}}, time.Now())
	require.NoError(t, err)
	require.NoError(t, st.SaveReview(ctx, rv))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: sources}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var got struct{ Code map[string]commentCode }
	require.Equal(t, get(t, ts, "/api/v1/reviews/"+rv.ID+"/code", &got), 200)

	c := got.Code[rv.Comments[0].ID]
	assert.Equal(t, c.Ref, commit)
	require.Len(t, c.Lines, 8)
	assert.Equal(t, c.Lines[0], codeLine{N: 7, Text: "line g"})
	assert.Equal(t, c.Lines[7], codeLine{N: 14, Text: "line n"})
	assert.Equal(t, c.URL, "https://github.com/acme/proj/blob/"+commit+"/app/a.go#L10-L11")

	secret := got.Code[rv.Comments[1].ID]
	require.Len(t, secret.Lines, 4)
	assert.False(t, strings.Contains(secret.Lines[3].Text, "hunter2"), "secrets are redacted")
	assert.Equal(t, secret.URL, "https://github.com/acme/proj/blob/"+commit+"/app/a.go#L21")

	assert.Equal(t, got.Code[rv.Comments[2].ID].Error, "file not found in the checkout")
	_, ok := got.Code[rv.Comments[3].ID]
	assert.False(t, ok, "comments without a line get no code")
}

func TestForgeLinker(t *testing.T) {
	s := &Server{orgs: mustOrgs(t)}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name, remote, ref, want string
	}{
		{"github range", "https://github.com/acme/proj.git", sha, "https://github.com/acme/proj/blob/" + sha + "/dir/a%20b.go#L3-L5"},
		{"gitlab subgroup", "https://gitlab.com/grp/sub/proj", sha, "https://gitlab.com/grp/sub/proj/-/blob/" + sha + "/dir/a%20b.go#L3-5"},
		{"codeberg commit", "https://codeberg.org/team/proj.git", sha, "https://codeberg.org/team/proj/src/commit/" + sha + "/dir/a%20b.go#L3-L5"},
		{"codeberg branch", "https://codeberg.org/team/proj.git", "main", "https://codeberg.org/team/proj/src/branch/main/dir/a%20b.go#L3-L5"},
		{"unknown host", "https://git.example.org/team/proj.git", sha, ""},
		{"ssh remote", "git@github.com:acme/proj.git", sha, ""},
		{"no ref", "https://github.com/acme/proj.git", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link := s.forgeLinker(tt.remote, tt.ref)
			if tt.want == "" {
				assert.True(t, link == nil)
				return
			}
			require.True(t, link != nil)
			assert.Equal(t, link("dir/a b.go", 3, 5), tt.want)
		})
	}
}

func TestCleanRelPath(t *testing.T) {
	for p, want := range map[string]bool{
		"app/a.go": true, "a.go": true, "": false, "/etc/passwd": false, "../x": false,
		"app/../../x": false, "app//a.go": false, `app\a.go`: false, "./a.go": false,
	} {
		assert.Equal(t, cleanRelPath(p), want, p)
	}
}

func mustOrgs(t *testing.T) *orgs {
	t.Helper()
	o, err := loadOrgs("")
	require.NoError(t, err)
	return o
}
