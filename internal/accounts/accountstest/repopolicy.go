package accountstest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunRepoPolicy checks tenant policy approvals, annotation quotas and provenance.
func RunRepoPolicy(t *testing.T, st accounts.Store) {
	ctx := t.Context()
	svc := accounts.NewService(st, true)
	u, org, err := svc.SignUp(ctx, "annotation-owner@example.com", "Annotation owner", pw)
	require.NoError(t, err)
	p := accounts.Principal{User: u, Via: "session"}
	other, err := svc.CreateOrg(ctx, p, "Annotation other", "")
	require.NoError(t, err)
	graph := "annotation/repo"
	for _, orgID := range []string{org.ID, other.ID} {
		require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: orgID, Graph: graph, AddedBy: u.ID, Visibility: accounts.Private, AddedAt: time.Now()}))
	}
	hash := sha256.Sum256([]byte(`{"version":1}`))
	approval := accounts.RepoPolicyApproval{OrgID: org.ID, Graph: graph, Digest: hex.EncodeToString(hash[:]), ConfigJSON: `{"version":1}`, Reason: "Approve local metadata annotation", AllowAnnotation: true}
	require.NoError(t, svc.SetRepoPolicy(ctx, p, approval))
	stored, err := st.GetRepoPolicy(ctx, org.ID, graph)
	require.NoError(t, err)
	assert.Equal(t, stored.ActorID, u.ID)
	assert.Equal(t, stored.Digest, approval.Digest)
	_, err = st.GetRepoPolicy(ctx, other.ID, graph)
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	keyP := p
	keyP.Via = "key"
	assert.True(t, errors.Is(svc.SetRepoPolicy(ctx, keyP, approval), accounts.ErrForbidden))
	bad := approval
	bad.Digest = "wrong"
	assert.True(t, errors.Is(svc.SetRepoPolicy(ctx, p, bad), accounts.ErrInvalidInput))

	reader, _, err := svc.SignUp(ctx, "annotation-reader@example.com", "Annotation reader", pw)
	require.NoError(t, err)
	require.NoError(t, Join(ctx, t, svc, p, org.ID, reader, accounts.Member))
	_, err = svc.SetRepoGrant(ctx, p, org.ID, graph, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: reader.ID}, accounts.Member)
	require.NoError(t, err)
	assert.True(t, errors.Is(svc.SetRepoPolicy(ctx, accounts.Principal{User: reader, Via: "session"}, approval), accounts.ErrForbidden))

	broken := accounts.NewService(failedRepoPolicyAudit{st}, true)
	bad = approval
	bad.Reason = "must roll back"
	require.Error(t, broken.SetRepoPolicy(ctx, p, bad))
	stored, err = st.GetRepoPolicy(ctx, org.ID, graph)
	require.NoError(t, err)
	assert.Equal(t, stored.Reason, approval.Reason)

	id, err := svc.RegisterExecutionSession(ctx, u.ID, org.ID, "annotation-contract-session")
	require.NoError(t, err)
	workCtx, err := execution.With(ctx, id)
	require.NoError(t, err)
	confidence := 0.8
	r := accounts.RepositoryAnnotation{OrgID: org.ID, Graph: graph, SymbolID: "symbol", Commit: "commit", Revision: "revision", Fingerprint: "fingerprint", PolicyDigest: approval.Digest, JobID: "job", OriginSessionID: id.SessionID, Summary: "Checks a request", Taxonomy: []string{"auth"}, Confidence: &confidence, UpdatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	assert.True(t, errors.Is(st.UpsertAnnotations(ctx, []accounts.RepositoryAnnotation{r}), execution.ErrMissing))
	require.NoError(t, st.UpsertAnnotations(workCtx, []accounts.RepositoryAnnotation{r}))
	duplicate := r
	duplicate.Summary = "must not replace idempotent result"
	require.NoError(t, st.UpsertAnnotations(workCtx, []accounts.RepositoryAnnotation{duplicate}))
	rows, err := st.ListAnnotations(ctx, org.ID, graph, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, rows[0].Summary, r.Summary)
	rows[0].Taxonomy[0] = "mutated"
	*rows[0].Confidence = 0.1
	rows, err = st.ListAnnotations(ctx, org.ID, graph, 100)
	require.NoError(t, err)
	assert.Equal(t, rows[0].Taxonomy[0], "auth")
	assert.Equal(t, *rows[0].Confidence, 0.8)
	rows, err = st.ListAnnotations(ctx, other.ID, graph, 100)
	require.NoError(t, err)
	assert.Len(t, rows, 0)
	foreign := r
	foreign.OrgID = other.ID
	assert.True(t, errors.Is(st.UpsertAnnotations(workCtx, []accounts.RepositoryAnnotation{r, foreign}), accounts.ErrInvalidInput))

	// Populate the bound through ordinary batches so both adapters enforce it.
	for start := 1; start < accounts.MaxRepositoryAnnotations; start += 100 {
		batch := []accounts.RepositoryAnnotation{}
		for i := start; i < min(start+100, accounts.MaxRepositoryAnnotations); i++ {
			entry := r
			entry.SymbolID = fmt.Sprintf("symbol-%05d", i)
			batch = append(batch, entry)
		}
		require.NoError(t, st.UpsertAnnotations(workCtx, batch))
	}
	full := r
	full.SymbolID = "overflow"
	assert.True(t, errors.Is(st.UpsertAnnotations(workCtx, []accounts.RepositoryAnnotation{full}), accounts.ErrAccessLimit))
	rows, err = st.ListAnnotations(ctx, org.ID, graph, accounts.MaxRepositoryAnnotations+1)
	require.NoError(t, err)
	assert.Len(t, rows, accounts.MaxRepositoryAnnotations)
	rows, err = st.ListAnnotations(ctx, org.ID, graph, 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	require.NoError(t, svc.DeleteRepoPolicy(ctx, p, org.ID, graph))
	assert.True(t, errors.Is(st.UpsertAnnotations(workCtx, []accounts.RepositoryAnnotation{r}), accounts.ErrForbidden))
	require.NoError(t, svc.SetRepoPolicy(ctx, p, approval))
	require.NoError(t, svc.RevokeExecutionSession(ctx, u.ID, org.ID, id.SessionID))
	assert.True(t, errors.Is(st.UpsertAnnotations(workCtx, []accounts.RepositoryAnnotation{r}), accounts.ErrExecutionSessionClosed))
	_, err = st.RemoveRepository(ctx, org.ID, graph)
	require.NoError(t, err)
	_, err = st.GetRepoPolicy(ctx, org.ID, graph)
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	rows, err = st.ListAnnotations(ctx, org.ID, graph, 100)
	require.NoError(t, err)
	assert.Len(t, rows, 0)
}

type failedRepoPolicyAudit struct{ accounts.Store }

func (s failedRepoPolicyAudit) PolicyTransaction(ctx context.Context, orgID string, fn func(accounts.Store) error) error {
	return s.Store.PolicyTransaction(ctx, orgID, func(tx accounts.Store) error { return fn(failedRepoPolicyAudit{tx}) })
}

func (s failedRepoPolicyAudit) AddEvent(context.Context, accounts.Event) (accounts.Event, error) {
	return accounts.Event{}, errors.New("audit unavailable")
}
