package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestSessionEditsSkipMergedWork: files a merge or a fast-forward pull
// brings in are not the session's edits; its own commits, conflicts it
// resolved and uncommitted edits are. The Stop hook's count follows.
func TestSessionEditsSkipMergedWork(t *testing.T) {
	ctx := context.Background()
	git := func(t *testing.T, dir string, args ...string) {
		t.Helper()
		_, err := gitx.Run(ctx, dir, append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		require.NoError(t, err, strings.Join(args, " "))
	}
	write := func(t *testing.T, dir, name, body string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	commit := func(t *testing.T, dir, msg string) {
		t.Helper()
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", msg)
	}
	// upstream is main with work of others: two new files and a changed one.
	upstream := func(t *testing.T, dir string) {
		t.Helper()
		git(t, dir, "checkout", "-q", "-b", "upstream")
		write(t, dir, "merged.go", "package demo\n\nfunc Merged() {}\n")
		write(t, dir, "docs.md", "# Docs\n")
		commit(t, dir, "others")
		write(t, dir, "demo.go", "package demo\n\nfunc Synced() int { return 7 }\n")
		commit(t, dir, "others again")
		git(t, dir, "checkout", "-q", "main")
	}
	edit := func(t *testing.T, dir string) {
		t.Helper()
		write(t, dir, "edit.go", "package demo\n\nfunc Edit() {}\n")
	}

	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) // before the session starts
		session func(t *testing.T, dir string)
		want    []string
	}{
		{
			name:  "merge commit brings in main",
			setup: upstream,
			session: func(t *testing.T, dir string) {
				git(t, dir, "merge", "-q", "--no-ff", "--no-edit", "upstream")
				edit(t, dir)
			},
			want: []string{"edit.go"},
		},
		{
			name:  "fast-forward pull",
			setup: upstream,
			session: func(t *testing.T, dir string) {
				git(t, dir, "merge", "-q", "--ff-only", "upstream")
				edit(t, dir)
			},
			want: []string{"edit.go"},
		},
		{
			name:  "own commit then a merge",
			setup: upstream,
			session: func(t *testing.T, dir string) {
				write(t, dir, "own.go", "package demo\n\nfunc Own() {}\n")
				commit(t, dir, "own")
				git(t, dir, "merge", "-q", "--no-edit", "upstream")
				edit(t, dir)
			},
			want: []string{"edit.go", "own.go"},
		},
		{
			name:  "merge in progress",
			setup: upstream,
			session: func(t *testing.T, dir string) {
				git(t, dir, "merge", "-q", "--no-ff", "--no-commit", "upstream")
				edit(t, dir)
			},
			want: []string{"edit.go"},
		},
		{
			name:  "conflict resolved by hand",
			setup: upstream,
			session: func(t *testing.T, dir string) {
				write(t, dir, "demo.go", "package demo\n\nfunc Synced() int { return 2 }\n")
				commit(t, dir, "own")
				_, err := gitx.Run(ctx, dir, "merge", "-q", "--no-edit", "upstream")
				require.Error(t, err, "the merge conflicts")
				write(t, dir, "demo.go", "package demo\n\nfunc Synced() int { return 9 }\n")
				commit(t, dir, "resolved")
			},
			want: []string{"demo.go"},
		},
		{
			name: "uncommitted edits only",
			session: func(t *testing.T, dir string) {
				edit(t, dir)
			},
			want: []string{"edit.go"},
		},
	}
	for _, tt := range tests {
		dir := gitRepo(t)
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			base := baseOf(ctx, dir)
			// The branch tracks where the session began, as a pushed branch would.
			git(t, dir, "branch", "start")
			git(t, dir, "branch", "-q", "--set-upstream-to=start")
			tt.session(t, dir)

			got := sessionEdits(ctx, dir, base)
			slices.Sort(got)
			assert.Equal(t, got, tt.want)

			// The Stop hook's "N file(s) changed" is this count.
			change := stopChanges(ctx, dir, base)
			assert.Equal(t, change.Files, len(tt.want))
			assert.Equal(t, change.Paths, tt.want)
		})
	}
}
