package gitx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The phase 4j audit fixtures now enforce the corrected path handling.
func TestAuditDiffCapabilities(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want []FileChange
	}{
		{
			name: "rename without edits retains paths",
			diff: "diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n",
			want: []FileChange{{Path: "new.go", OldPath: "old.go"}},
		},
		{
			name: "rename with edits retains paths",
			diff: "diff --git a/old.go b/new.go\nrename from old.go\nrename to new.go\n--- a/old.go\n+++ b/new.go\n@@ -2 +2 @@\n-old\n+new\n",
			want: []FileChange{{Path: "new.go", OldPath: "old.go", Ranges: []LineRange{{Start: 2, End: 2}}}},
		},
		{
			name: "quoted tab is decoded",
			diff: "diff --git \"a/tab\\tname.go\" \"b/tab\\tname.go\"\n--- \"a/tab\\tname.go\"\n+++ \"b/tab\\tname.go\"\n@@ -1 +1 @@\n-old\n+new\n",
			want: []FileChange{{Path: "tab\tname.go", Ranges: []LineRange{{Start: 1, End: 1}}}},
		},
		{
			name: "repeated blocks retain separate hunk positions",
			diff: "--- a/a.go\n+++ b/a.go\n@@ -2 +2 @@\n-old\n+new\n@@ -20 +20 @@\n-old\n+new\n",
			want: []FileChange{{Path: "a.go", Ranges: []LineRange{{Start: 2, End: 2}, {Start: 20, End: 20}}}},
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

func TestAuditDiffOversizedLineFails(t *testing.T) {
	diff := "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n+" + strings.Repeat("x", maxDiffLine) + "\n"
	_, err := ParseDiff([]byte(diff))
	require.Error(t, err)
}

func BenchmarkAuditParseDiff(b *testing.B) {
	for _, files := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("files_%d", files), func(b *testing.B) {
			var corpus strings.Builder
			for i := range files {
				fmt.Fprintf(&corpus, "diff --git a/f%d.go b/f%d.go\n--- a/f%d.go\n+++ b/f%d.go\n@@ -1 +1 @@\n-old\n+new\n", i, i, i, i)
			}
			data := []byte(corpus.String())
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				changes, err := ParseDiff(data)
				if err != nil || len(changes) != files {
					b.Fatalf("parsed %d files: %v", len(changes), err)
				}
			}
		})
	}
}
