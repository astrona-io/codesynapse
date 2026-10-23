package fixes

import (
	"fmt"
	"go/parser"
	"go/token"
	"net/http"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The audit records missing validation so later work can replace these baseline
// expectations with explicit refusals before a generated branch is pushed.
func TestAuditRewriteValidationGaps(t *testing.T) {
	orig := []byte("// Copyright example\npackage app\n\nfunc Run() { first(); second() }\n")
	tests := []struct {
		name string
		out  string
	}{
		{"invalid syntax", "package app\nfunc Run( {\n"},
		{"copyright removed", "package app\n\nfunc Run() { second() }\n"},
		{"unrelated replacement", "package app\n\nfunc Different() {}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, plausible(orig, []byte(tt.out)))
		})
	}
	t.Run("multiple fences become one file", func(t *testing.T) {
		out, err := extractFile("```go\npackage app\n```\nexplanation\n```go\nfunc Other() {}\n```\n")
		require.NoError(t, err)
		assert.Contains(t, out, "explanation")
		_, err = parser.ParseFile(token.NewFileSet(), "a.go", out, parser.AllErrors)
		require.Error(t, err)
	})
}

func TestAuditMalformedRewriteReachesPushCallback(t *testing.T) {
	ctx := fixContext(t)
	f := setup(ctx, t, "```go\npackage app\nfunc Run( {\n```\n", http.StatusCreated)
	_, err := f.r.Run(ctx, f.req)
	require.NoError(t, err)
	assert.Len(t, f.pushed, 1)
}

func TestAuditReviewRevisionIsNotComparedWithFixBase(t *testing.T) {
	ctx := fixContext(t)
	f := setup(ctx, t, fixed, http.StatusCreated)
	rv, err := f.st.GetReview(ctx, f.req.ReviewID)
	require.NoError(t, err)
	rv.Head = strings.Repeat("a", 40)
	require.NoError(t, f.st.SaveReview(ctx, rv))
	assert.NotEqual(t, rv.Head, f.req.Base)
	_, err = f.r.Run(ctx, f.req)
	require.NoError(t, err)
	assert.Len(t, f.pushed, 1)
}

func BenchmarkAuditRewriteEnvelope(b *testing.B) {
	for _, size := range []int{4096, 65536, maxFileBytes} {
		b.Run(fmt.Sprintf("bytes_%d", size), func(b *testing.B) {
			orig := []byte(strings.Repeat("// retained comment\n", size/19+1)[:size])
			answer := "```go\n" + string(orig) + "// changed\n```\n"
			b.SetBytes(int64(len(answer)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				out, err := extractFile(answer)
				if err != nil {
					b.Fatal(err)
				}
				if err := plausible(orig, []byte(out)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
