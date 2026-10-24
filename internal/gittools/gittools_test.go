package gittools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	_, err := gitx.Run(context.Background(), dir, args...)
	require.NoError(t, err)
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

func commit(t *testing.T, dir, msg, author string) {
	t.Helper()
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name="+author, "-c", "user.email="+strings.ToLower(author)+"@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", msg)
}

func repo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	return dir
}

const goFile = `package demo

func First() int {
	return 1
}

func Second() int {
	return 2
}

func Third() int {
	return 3
}
`

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestStatusAndDiffSummaryOfAChangedCheckout(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()

	empty, err := Status(ctx, dir)
	require.NoError(t, err)
	assert.True(t, empty.Clean, "an empty repository is clean")

	write(t, dir, "demo.go", goFile)
	write(t, dir, "notes.md", "one\ntwo\n")
	commit(t, dir, "initial", "Ada")

	clean, err := Status(ctx, dir)
	require.NoError(t, err)
	assert.True(t, clean.Clean)
	assert.Equal(t, clean.Branch, "main")
	assert.Equal(t, len(clean.Files), 0)
	assert.Equal(t, clean.Next, "")

	// An unstaged edit, a staged edit, a deletion and an untracked file.
	write(t, dir, "demo.go", strings.Replace(goFile, "return 2", "return 22\n\t// changed", 1))
	write(t, dir, "notes.md", "one\ntwo\nthree\n")
	run(t, dir, "add", "notes.md")
	write(t, dir, "fresh/new.go", "package fresh\n\nfunc New() {}\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "demo.go")))
	write(t, dir, "demo.go", strings.Replace(goFile, "return 2", "return 22\n\t// changed", 1))

	st, err := Status(ctx, dir)
	require.NoError(t, err)
	assert.False(t, st.Clean)
	by := map[string]StatusFile{}
	for _, f := range st.Files {
		by[f.Path] = f
	}
	require.Len(t, st.Files, 3)
	assert.Equal(t, by["demo.go"].Status, "M")
	assert.True(t, by["demo.go"].Unstaged && !by["demo.go"].Staged)
	assert.Equal(t, by["demo.go"].Added, 2)
	assert.Equal(t, by["demo.go"].Removed, 1)
	assert.True(t, by["notes.md"].Staged && !by["notes.md"].Unstaged)
	assert.Equal(t, by["notes.md"].Added, 1)
	assert.Equal(t, by["fresh/new.go"].Status, "?")
	assert.Equal(t, by["fresh/new.go"].Added, 3, "untracked files count their lines")
	assert.Equal(t, st.Totals.Untracked, 1)
	assert.Equal(t, st.Totals.Added, 6)
	assert.Contains(t, st.Next, "git_diff_summary")

	d, err := DiffSummary(ctx, dir, DiffIn{})
	require.NoError(t, err)
	assert.Equal(t, d.Against, "HEAD")
	files := map[string]DiffFile{}
	for _, f := range d.Files {
		files[f.Path] = f
	}
	require.Len(t, d.Files, 3)
	require.Len(t, files["demo.go"].Hunks, 1)
	h := files["demo.go"].Hunks[0]
	assert.Equal(t, h.New, "8-9")
	assert.Equal(t, h.Old, "8")
	assert.Equal(t, files["notes.md"].Status, "M")
	assert.Equal(t, files["fresh/new.go"].Status, "?")
	assert.Equal(t, files["fresh/new.go"].Hunks[0].New, "1-3")
	assert.NotContains(t, asJSON(t, d), "return 22", "no patch text")

	// Limiting to a path.
	only, err := DiffSummary(ctx, dir, DiffIn{Paths: []string{"notes.md"}})
	require.NoError(t, err)
	require.Len(t, only.Files, 1)
	assert.Equal(t, only.Files[0].Path, "notes.md")
}

func TestDiffSummaryAgainstABaseBranch(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	write(t, dir, "a.go", "package a\n")
	commit(t, dir, "base", "Ada")
	run(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "b.go", "package b\n\nfunc B() {}\n")
	commit(t, dir, "feature work", "Bob")
	write(t, dir, "c.go", "package c\n")

	d, err := DiffSummary(ctx, dir, DiffIn{Base: "main"})
	require.NoError(t, err)
	assert.Contains(t, d.Against, "main")
	var paths []string
	for _, f := range d.Files {
		paths = append(paths, f.Path)
	}
	assert.Equal(t, strings.Join(paths, ","), "b.go,c.go", "committed and uncommitted work since the branch left main")

	_, err = DiffSummary(ctx, dir, DiffIn{Base: "--output=/tmp/x"})
	assert.ErrorIs(t, err, ErrInvalid)
	_, err = DiffSummary(ctx, dir, DiffIn{Paths: []string{"../outside"}})
	assert.ErrorIs(t, err, ErrInvalid)
}

func TestBlameGroupsLinesByCommit(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	write(t, dir, "demo.go", goFile)
	commit(t, dir, "add the demo", "Ada")
	write(t, dir, "demo.go", strings.Replace(goFile, "return 2", "return 22", 1))
	commit(t, dir, "second returns 22", "Bob")
	write(t, dir, "demo.go", strings.Replace(strings.Replace(goFile, "return 2", "return 22", 1), "return 3", "return 33", 1))

	b, err := Blame(ctx, dir, BlameIn{File: "demo.go", Start: 1, End: 12})
	require.NoError(t, err)
	var got []string
	for _, bl := range b.Blocks {
		got = append(got, bl.Lines+" "+bl.Author+" "+bl.Commit+" "+bl.Subject)
	}
	require.Len(t, b.Blocks, 4, strings.Join(got, "\n"))
	assert.Equal(t, b.Blocks[0].Lines, "1-7")
	assert.Equal(t, b.Blocks[0].Author, "Ada")
	assert.Equal(t, b.Blocks[0].Subject, "add the demo")
	assert.Equal(t, b.Blocks[1].Lines, "8")
	assert.Equal(t, b.Blocks[1].Author, "Bob")
	assert.Equal(t, b.Blocks[2].Lines, "9-11")
	assert.Equal(t, b.Blocks[2].Author, "Ada", "the same commit again after another one's line is a block of its own")
	assert.Equal(t, b.Blocks[3].Commit, "uncommitted", "the edit not committed yet: "+strings.Join(got, "|"))
	assert.Equal(t, b.Blocks[3].Lines, "12")
	assert.Equal(t, b.Blocks[3].Author, "")
	assert.NotContains(t, asJSON(t, b), "@example.com", "no e-mail addresses")

	one, err := Blame(ctx, dir, BlameIn{File: "demo.go", Start: 8})
	require.NoError(t, err)
	require.Len(t, one.Blocks, 1)
	assert.Equal(t, one.Blocks[0].Lines, "8")

	for name, in := range map[string]BlameIn{
		"escape":   {File: "../x", Start: 1},
		"absolute": {File: "/etc/passwd", Start: 1},
		"option":   {File: "-p", Start: 1},
		"order":    {File: "demo.go", Start: 5, End: 2},
		"zero":     {File: "demo.go", Start: 0},
		"too many": {File: "demo.go", Start: 1, End: MaxBlameLines + 1},
	} {
		_, err := Blame(ctx, dir, in)
		assert.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestLogIsOneLinePerCommit(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	l, err := Log(ctx, dir, LogIn{})
	require.NoError(t, err)
	assert.Equal(t, len(l.Commits), 0, "no commit yet")

	write(t, dir, "a.go", "package a\n")
	commit(t, dir, "first", "Ada")
	write(t, dir, "a.go", "package a\n\nfunc A() {}\n")
	write(t, dir, "b.go", "package b\n")
	write(t, dir, "c.md", "x\ny\n")
	commit(t, dir, "second: token ghp_"+strings.Repeat("a1B2", 9), "Bob")

	l, err = Log(ctx, dir, LogIn{})
	require.NoError(t, err)
	require.Len(t, l.Commits, 2)
	second := l.Commits[0]
	assert.Equal(t, second.Author, "Bob")
	assert.Equal(t, second.Files, 3)
	assert.Equal(t, second.Added, 5)
	assert.Equal(t, len(second.Touched), 3)
	assert.Equal(t, second.Touched[0], "a.go", "the file changed most comes first, ties by name")
	assert.NotContains(t, second.Subject, "ghp_", "subjects are redacted")
	assert.Equal(t, l.Commits[1].Subject, "first")

	one, err := Log(ctx, dir, LogIn{Path: "b.go", Limit: 5})
	require.NoError(t, err)
	require.Len(t, one.Commits, 1)
	assert.Equal(t, one.Commits[0].Subject[:6], "second")
	assert.Equal(t, one.Path, "b.go")

	capped, err := Log(ctx, dir, LogIn{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, len(capped.Commits), 1)

	for name, in := range map[string]LogIn{
		"limit":  {Limit: MaxLogEntries + 1},
		"ref":    {Ref: "--all"},
		"path":   {Path: "../x"},
		"range":  {Ref: "a..b"},
		"option": {Path: "-n"},
	} {
		_, err := Log(ctx, dir, in)
		assert.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestSmallAnswers(t *testing.T) {
	dir := repo(t)
	ctx := context.Background()
	for i := range 30 {
		write(t, dir, filepath.Join("pkg", "f"+strings.Repeat("x", i)+".go"), "package pkg\n\nfunc F() {}\n")
	}
	commit(t, dir, "many files", "Ada")
	for i := range 30 {
		write(t, dir, filepath.Join("pkg", "f"+strings.Repeat("x", i)+".go"), "package pkg\n\nfunc F() int { return 1 }\n")
	}
	st, err := Status(ctx, dir)
	require.NoError(t, err)
	d, err := DiffSummary(ctx, dir, DiffIn{})
	require.NoError(t, err)
	l, err := Log(ctx, dir, LogIn{})
	require.NoError(t, err)
	// The point of the tools: a few hundred tokens for what raw git output
	// spreads over thousands. 4 bytes is the token estimate used elsewhere.
	for name, v := range map[string]any{"status": st, "diff": d, "log": l} {
		assert.True(t, len(asJSON(t, v))/4 < 1500, name+" stays small")
	}
}
