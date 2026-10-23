package gitx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestDiffPaths(t *testing.T) {
	tests := []struct {
		name, diff string
		want       []FileChange
	}{
		{
			name: "octal UTF-8 and escaped quotes",
			diff: "--- \"a/\\303\\251\\\".go\"\n+++ \"b/\\303\\251\\\".go\"\n@@ -1 +1 @@\n-old\n+new\n",
			want: []FileChange{{Path: "é\".go", Ranges: []LineRange{{1, 1}}}},
		},
		{
			name: "significant trailing space",
			diff: "--- a/file name.go \n+++ b/file name.go \n@@ -1 +1 @@\n-old\n+new\n",
			want: []FileChange{{Path: "file name.go ", Ranges: []LineRange{{1, 1}}}},
		},
		{
			name: "plain headers with timestamps",
			diff: "--- a/file name.go\t2026-10-23\n+++ b/file name.go\t2026-10-23\n@@ -1 +1 @@\n-old\n+new\n",
			want: []FileChange{{Path: "file name.go", Ranges: []LineRange{{1, 1}}}},
		},
		{
			name: "rename keeps literal a prefix",
			diff: "diff --git a/a/old file.go b/b/new file.go\nrename from a/old file.go\nrename to b/new file.go\n",
			want: []FileChange{{Path: "b/new file.go", OldPath: "a/old file.go"}},
		},
		{
			name: "rename with ambiguous spaces in Git header",
			diff: "diff --git a/a b/old.go b/c b/new.go\nrename from a b/old.go\nrename to c b/new.go\n",
			want: []FileChange{{Path: "c b/new.go", OldPath: "a b/old.go"}},
		},
		{
			name: "copy metadata",
			diff: "diff --git a/old.go b/new.go\ncopy from old.go\ncopy to new.go\n",
			want: []FileChange{{Path: "new.go", OldPath: "old.go"}},
		},
		{
			name: "mode only with ambiguous spaces",
			diff: "diff --git a/a b/name.go b/a b/name.go\nold mode 100644\nnew mode 100755\n",
			want: []FileChange{{Path: "a b/name.go"}},
		},
		{
			name: "binary addition",
			diff: "diff --git a/image.png b/image.png\nnew file mode 100644\nBinary files /dev/null and b/image.png differ\n",
			want: []FileChange{{Path: "image.png", Added: true}},
		},
		{
			name: "binary deletion",
			diff: "diff --git \"a/image\\tname.png\" \"b/image\\tname.png\"\ndeleted file mode 100644\nBinary files a/image and /dev/null differ\n",
			want: []FileChange{{Path: "image\tname.png", Deleted: true}},
		},
		{
			name: "hunk payload resembling file headers",
			diff: "--- a/real.go\n+++ b/real.go\n@@ -1,2 +1,2 @@\n--- a/fake.go\n+++ b/fake.go\n same\n",
			want: []FileChange{{Path: "real.go", Ranges: []LineRange{{1, 2}}}},
		},
		{
			name: "multiple plain unified files",
			diff: "--- a/first.go\n+++ b/first.go\n@@ -1 +1 @@\n-old\n+new\n--- a/second.go\n+++ b/second.go\n@@ -2 +2,0 @@\n-old\n",
			want: []FileChange{{Path: "first.go", Ranges: []LineRange{{1, 1}}}, {Path: "second.go", Ranges: []LineRange{{2, 2}}}},
		},
		{
			name: "deletion at file start",
			diff: "--- a/first.go\n+++ b/first.go\n@@ -1,2 +0,0 @@\n-a\n-b\n",
			want: []FileChange{{Path: "first.go", Ranges: []LineRange{{1, 1}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDiff([]byte(tt.diff))
			require.NoError(t, err)
			assert.Equal(t, got, tt.want)
		})
	}
}

func TestDiffRejectsMalformedHeaders(t *testing.T) {
	tests := []string{
		"diff --git malformed\n",
		"diff --git \n--- a/a.go\n+++ b/a.go\n",
		"diff --git a/a.go b/b.go\nrename from a.go\nrename to c.go\n--- a/a.go\n+++ b/b.go\n",
		"diff --git a/a.go b/a.go\n--- a/b.go\n+++ b/b.go\n",
		"--- a/a.go\n",
		"+++ b/a.go\n",
		"--- a/a.go\n+++ b/a.go\n+++ b/other.go\n",
		"--- /dev/null\n+++ /dev/null\n",
		"--- \"a/unterminated\n+++ b/a.go\n",
		"--- \"a/a.go\" trailing\n+++ b/a.go\n",
		"--- \"a/\\x61.go\"\n+++ b/a.go\n",
		"--- \"a/\\400.go\"\n+++ b/a.go\n",
		"--- \"a/\\000.go\"\n+++ b/a.go\n",
		"--- a/../a.go\n+++ b/a.go\n",
		"--- /absolute.go\n+++ b/a.go\n",
		"rename from a.go\nrename to b.go\n",
		"diff --git a/a.go b/b.go\nrename from a.go\n",
		"diff --git a/a.go b/b.go\nrename to b.go\n",
		"diff --git a/a.go b/b.go\nrename from a.go\nrename to c.go\n",
		"diff --git a/a.go b/a.go\n@@ -1 +1 @@\n",
	}
	for _, diff := range tests {
		t.Run(diff, func(t *testing.T) {
			changes, err := ParseDiff([]byte(diff))
			require.Error(t, err)
			assert.Len(t, changes, 0)
		})
	}
}

func TestDiffHunkRangeBounds(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	bad := []string{
		"@@ -x +1 @@", "@@ +1 +1 @@", "@@ -1 -1 @@",
		"@@ -1 +1", "@@ -1 +1 @", "@@@ -1 +1 @@@",
		"@@ -1, +1 @@", "@@ -1 +1, @@", "@@ --1 +1 @@",
		"@@ -1 ++1 @@", "@@ -0 +1 @@", "@@ -1 +0 @@",
		fmt.Sprintf("@@ -%d,2 +1 @@", maxInt),
		fmt.Sprintf("@@ -1 +%d,2 @@", maxInt),
		"@@ -99999999999999999999999999 +1 @@",
		"@@ -1 +1,99999999999999999999999999 @@",
	}
	for _, header := range bad {
		t.Run(header, func(t *testing.T) {
			changes, err := ParseDiff([]byte("--- a/a.go\n+++ b/a.go\n" + header + "\n"))
			require.Error(t, err)
			assert.Len(t, changes, 0)
		})
	}
	changes, err := ParseDiff(fmt.Appendf(nil, "--- a/a.go\n+++ b/a.go\n@@ -%d +%d @@\n", maxInt, maxInt))
	require.NoError(t, err)
	assert.Equal(t, changes[0].Ranges, []LineRange{{maxInt, maxInt}})
}

func TestDiffRejectsOversizedInput(t *testing.T) {
	_, err := ParseDiff([]byte(strings.Repeat("\n", MaxDiffBytes+1)))
	require.Error(t, err)
}

func TestDiffGitPathRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	_, err := Run(ctx, dir, "init", "-q", "-b", "main")
	require.NoError(t, err)
	paths := []string{"space name.go", "trailing.go ", "tab\tname.go", "é.go", "quote\".go", "line\nname.go"}
	for _, name := range paths {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("old\n"), 0o600))
	}
	_, err = CommitFiles(ctx, dir, paths, "initial files", Author{Name: "test", Email: "test@example.com"})
	require.NoError(t, err)
	for _, name := range paths {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("new\n"), 0o600))
	}
	diff, err := Run(ctx, dir, "diff", "--no-ext-diff", "--no-textconv", "--unified=0", "--")
	require.NoError(t, err)
	changes, err := ParseDiff(diff)
	require.NoError(t, err)
	require.Len(t, changes, len(paths))
	got := make(map[string]bool)
	for _, change := range changes {
		got[change.Path] = true
		assert.Equal(t, change.Ranges, []LineRange{{1, 1}})
	}
	for _, name := range paths {
		assert.True(t, got[name], name)
	}
}
