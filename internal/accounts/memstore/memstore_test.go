package memstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/accounts/accountstest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestContract(t *testing.T) {
	st := New()
	accountstest.Run(t, st)
	accountstest.RunConcurrentAudit(t, st)
}

func TestAuditTamperDetected(t *testing.T) {
	ctx := context.Background()
	st := New()
	svc := accounts.NewService(st, true)
	for i := range 3 {
		_, err := svc.Record(ctx, accounts.Event{Action: fmt.Sprintf("a%d", i)})
		require.NoError(t, err)
	}
	st.events[1].Detail = "edited afterwards"
	v, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.False(t, v.OK)
	assert.Equal(t, v.BrokenAt, int64(2))
	st.events[1].Detail = ""
	st.events = append(st.events[:1], st.events[2:]...) // an entry removed
	v, err = accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.False(t, v.OK)
	assert.Equal(t, v.BrokenAt, int64(3))
}
