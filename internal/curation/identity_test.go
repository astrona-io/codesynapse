package curation

import (
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIdentityReviewValidation(t *testing.T) {
	good := IdentityReviewInput{From: "sym-" + strings.Repeat("a", 32), To: "sym-" + strings.Repeat("b", 32), Relation: "same-symbol", Revision: "abc", Reason: "Reviewed token ghp_abcdefghijklmnopqrstuvwxyz0123456789"}
	it, err := PrepareIdentityReview("r", "o", "u", good, time.Now())
	require.NoError(t, err)
	assert.False(t, strings.Contains(it.IdentityReview.Reason, "ghp_abcdefghijklmnopqrstuvwxyz0123456789"))
	for _, tt := range []struct {
		name string
		edit func(*IdentityReviewInput)
	}{
		{"same ID", func(in *IdentityReviewInput) { in.To = in.From }},
		{"invalid ID", func(in *IdentityReviewInput) { in.From = "sym-" + strings.Repeat("z", 32) }},
		{"unbounded reason", func(in *IdentityReviewInput) { in.Reason = strings.Repeat("x", 1001) }},
		{"missing revision evidence", func(in *IdentityReviewInput) { in.Revision = "" }},
		{"unknown relation", func(in *IdentityReviewInput) { in.Relation = "approve" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := good
			tt.edit(&in)
			_, err := PrepareIdentityReview("r", "o", "u", in, time.Now())
			assert.ErrorIs(t, err, ErrInvalid)
		})
	}
}
