package api

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// A client that hangs up before its entry is written must not lose it.
func TestAuditContextOutlivesTheRequest(t *testing.T) {
	req, cancel := context.WithCancel(accounts.WithRequestInfo(t.Context(), accounts.RequestInfo{RequestID: "r1"}))
	cancel()
	ctx, done := auditContext(req)
	defer done()
	assert.NoError(t, ctx.Err())
	_, bounded := ctx.Deadline()
	assert.True(t, bounded, "the write is still bounded")
	assert.Equal(t, accounts.RequestInfoFrom(ctx).RequestID, "r1")
}
