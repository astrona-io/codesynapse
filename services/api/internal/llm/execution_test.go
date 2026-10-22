package llm

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func modelContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := execution.With(t.Context(), execution.Identity{TenantID: "org-fixture", SubjectID: "user-fixture", SessionID: "chat-fixture", ParentSessionID: "chat-fixture"})
	require.NoError(t, err)
	return ctx
}

func TestRegistryRequiresExecutionBeforeProviderLookup(t *testing.T) {
	r := &Registry{}
	_, err := r.Complete(t.Context(), "missing", Request{})
	assert.ErrorIs(t, err, execution.ErrMissing)
	_, err = r.Embed(t.Context(), "missing", []string{"entity"})
	assert.ErrorIs(t, err, execution.ErrMissing)
}
