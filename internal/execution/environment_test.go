package execution

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestEnvironmentDropsInheritedRuntimeAndDoesNotMixRequests(t *testing.T) {
	base := []string{"PATH=/bin", "CODESYNAPSE_SESSION_ID=spoofed", "CODESYNAPSE_TENANT_ID=foreign", "CODESYNAPSE_SUBJECT_ID=foreign", "CODESYNAPSE_PARENT_SESSION_ID=spoofed"}
	before := slices.Clone(base)
	plain, err := Environment(context.Background(), base)
	require.NoError(t, err)
	assert.Equal(t, plain, []string{"PATH=/bin"})
	var wg sync.WaitGroup
	for _, session := range []string{"chat-a", "chat-b"} {
		wg.Go(func() {
			id := Identity{TenantID: "org-fixture", SubjectID: "user-fixture", SessionID: session, ParentSessionID: session}
			ctx, err := With(t.Context(), id)
			require.NoError(t, err)
			for range 100 {
				env, err := Environment(ctx, base)
				require.NoError(t, err)
				assert.Contains(t, env, "CODESYNAPSE_SESSION_ID="+session)
				assert.Contains(t, env, "CODESYNAPSE_PARENT_SESSION_ID="+session)
				assert.False(t, slices.Contains(env, "CODESYNAPSE_SESSION_ID=spoofed"))
			}
		})
	}
	wg.Wait()
	assert.Equal(t, base, before)
}
