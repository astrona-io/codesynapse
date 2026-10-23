package fixes

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The audit gaps must now be refused before publishing a generated branch.
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
			require.Error(t, validateRewrite("a.go", orig, []byte(tt.out), []reviews.CommentItem{{File: "a.go", Line: 4, Status: reviews.Accepted}}))
		})
	}
	t.Run("multiple fences are refused", func(t *testing.T) {
		_, err := extractFile("```go\npackage app\n```\nexplanation\n```go\nfunc Other() {}\n```\n")
		require.Error(t, err)
	})
}

func TestAuditMalformedRewriteCannotReachPush(t *testing.T) {
	ctx := fixContext(t)
	f := setup(ctx, t, "```go\npackage app\nfunc Run( {\n```\n", http.StatusCreated)
	_, err := f.r.Run(ctx, f.req)
	require.Error(t, err)
	assert.Len(t, f.pushed, 0)
}

func TestAuditReviewRevisionMustMatchFixBase(t *testing.T) {
	ctx := fixContext(t)
	f := setup(ctx, t, fixed, http.StatusCreated)
	rv, err := f.st.GetReview(ctx, f.req.ReviewID)
	require.NoError(t, err)
	rv.Head = strings.Repeat("a", 40)
	require.NoError(t, f.st.SaveReview(ctx, rv))
	assert.NotEqual(t, rv.Head, f.req.Base)
	_, err = f.r.Run(ctx, f.req)
	require.Error(t, err)
	assert.Len(t, f.pushed, 0)
}

func BenchmarkAuditRewriteEnvelope(b *testing.B) {
	for _, size := range []int{4096, 65536, maxFileBytes - 32} {
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
