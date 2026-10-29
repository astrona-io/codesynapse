package curation

import (
	"reflect"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/arch"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Generated documentation pages stay drafts until a person verifies them by
// committing a file: a proposal always starts with its type's draft word, that
// word binds nothing, and the input has no way to set another status.
func TestProposalStaysDraft(t *testing.T) {
	for _, typ := range proposalTypes {
		t.Run(typ, func(t *testing.T) {
			it, err := PrepareProposal("repo", "org", "user", "agent", ProposalInput{Type: typ, Title: "Use one queue", Body: "Status: accepted"}, time.Now())
			require.NoError(t, err)
			require.NotNil(t, it.Proposal)
			assert.Equal(t, it.Proposal.Status, DraftStatus(typ))
			assert.Equal(t, it.Proposal.Status, arch.StatusesOf(typ)[0], "the first word of the lifecycle")
			assert.False(t, arch.Binding(typ, it.Proposal.Status), "a draft decides nothing")
		})
	}
	in := reflect.TypeFor[ProposalInput]()
	for i := range in.NumField() {
		assert.NotEqual(t, in.Field(i).Name, "Status", "callers cannot choose a status")
	}
}
