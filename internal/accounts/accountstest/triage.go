package accountstest

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunFindingTriage checks triage decisions: editors only, per organisation,
// audited in the same transaction, validated, bounded and gone with the
// repository entry.
func RunFindingTriage(t *testing.T, st accounts.Store) {
	ctx := t.Context()
	svc := accounts.NewService(st, true)
	owner, org, err := svc.SignUp(ctx, "triage-owner@example.com", "Triage owner", pw)
	require.NoError(t, err)
	p := accounts.Principal{User: owner, Via: "session"}
	other, err := svc.CreateOrg(ctx, p, "Triage other", "")
	require.NoError(t, err)
	graph := "triage/repo"
	for _, orgID := range []string{org.ID, other.ID} {
		require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: orgID, Graph: graph, AddedBy: owner.ID, Visibility: accounts.Private, AddedAt: time.Now()}))
	}
	fp := strings.Repeat("a", 64)
	base := accounts.FindingTriage{OrgID: org.ID, Graph: graph, Fingerprint: fp, Rule: "go-sql-injection", File: "db/query.go", State: accounts.TriageAcceptedRisk, Reason: "Input is a constant\nfrom config"}

	got, err := svc.SetFindingTriage(ctx, p, base)
	require.NoError(t, err)
	assert.Equal(t, got.ActorID, owner.ID)
	assert.Equal(t, got.Reason, "Input is a constant from config")
	assert.False(t, got.CreatedAt.IsZero())
	rows, err := st.ListFindingTriage(ctx, org.ID, graph, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, rows[0].ActorName, "Triage owner")
	assert.Equal(t, rows[0].State, accounts.TriageAcceptedRisk)
	rows, err = st.ListFindingTriage(ctx, other.ID, graph, 10)
	require.NoError(t, err)
	assert.Len(t, rows, 0, "another organisation sharing the graph sees nothing")

	events, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: "finding.triage.set"})
	require.NoError(t, err)
	assert.Len(t, events, 1)

	cases := []struct {
		name string
		edit func(*accounts.FindingTriage)
		ref  string
		err  error
	}{
		{name: "fixed by commit", edit: func(x *accounts.FindingTriage) { x.State, x.Reference = accounts.TriageFixedBy, "ABCDEF1234" }, ref: "abcdef1234"},
		{name: "fixed by pull request", edit: func(x *accounts.FindingTriage) { x.State, x.Reference = accounts.TriageFixedBy, "412" }, ref: "#412"},
		{name: "false positive", edit: func(x *accounts.FindingTriage) { x.State = accounts.TriageFalsePositive }},
		{name: "fixed by without reference", edit: func(x *accounts.FindingTriage) { x.State = accounts.TriageFixedBy }, err: accounts.ErrInvalidInput},
		{name: "fixed by with a branch name", edit: func(x *accounts.FindingTriage) { x.State, x.Reference = accounts.TriageFixedBy, "main; rm" }, err: accounts.ErrInvalidInput},
		{name: "reference on accepted risk", edit: func(x *accounts.FindingTriage) { x.Reference = "#1" }, err: accounts.ErrInvalidInput},
		{name: "unknown state", edit: func(x *accounts.FindingTriage) { x.State = "ignored" }, err: accounts.ErrInvalidInput},
		{name: "empty reason", edit: func(x *accounts.FindingTriage) { x.Reason = "  " }, err: accounts.ErrInvalidInput},
		{name: "long reason", edit: func(x *accounts.FindingTriage) { x.Reason = strings.Repeat("x", accounts.MaxTriageReason+1) }, err: accounts.ErrInvalidInput},
		{name: "bad fingerprint", edit: func(x *accounts.FindingTriage) { x.Fingerprint = "../etc" }, err: accounts.ErrInvalidInput},
		{name: "expiry in the past", edit: func(x *accounts.FindingTriage) { at := time.Now().Add(-time.Hour); x.ExpiresAt = &at }, err: accounts.ErrInvalidInput},
		{name: "unknown repository", edit: func(x *accounts.FindingTriage) { x.Graph = "triage/missing" }, err: accounts.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.edit(&in)
			got, err := svc.SetFindingTriage(ctx, p, in)
			if tc.err != nil {
				assert.True(t, errors.Is(err, tc.err), "got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, got.Reference, tc.ref)
		})
	}

	// Editors may triage, viewers may not; API keys act like sessions.
	editor, _, err := svc.SignUp(ctx, "triage-editor@example.com", "Triage editor", pw)
	require.NoError(t, err)
	viewer, _, err := svc.SignUp(ctx, "triage-viewer@example.com", "Triage viewer", pw)
	require.NoError(t, err)
	for _, u := range []accounts.User{editor, viewer} {
		_, err = svc.AddMember(ctx, p, org.ID, u.Email, accounts.Member)
		require.NoError(t, err)
	}
	_, err = svc.SetRepoGrant(ctx, p, org.ID, graph, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: editor.ID}, accounts.Member)
	require.NoError(t, err)
	_, err = svc.SetRepoGrant(ctx, p, org.ID, graph, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: viewer.ID}, accounts.Viewer)
	require.NoError(t, err)
	_, err = svc.SetFindingTriage(ctx, accounts.Principal{User: viewer, Via: "session"}, base)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "viewer: %v", err)
	assert.True(t, errors.Is(svc.ClearFindingTriage(ctx, accounts.Principal{User: viewer, Via: "session"}, org.ID, graph, fp), accounts.ErrForbidden))
	_, err = svc.SetFindingTriage(ctx, accounts.Principal{User: editor, Via: "automation", OrgID: org.ID}, base)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "automation: %v", err)
	_, err = svc.SetFindingTriage(ctx, accounts.Principal{User: editor, Via: "session"}, base)
	require.NoError(t, err)
	rows, err = st.ListFindingTriage(ctx, org.ID, graph, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "a second decision replaces the first")
	assert.Equal(t, rows[0].ActorID, editor.ID)

	// A failed audit rolls the change back.
	broken := accounts.NewService(failedRepoPolicyAudit{st}, true)
	changed := base
	changed.Reason = "must roll back"
	_, err = broken.SetFindingTriage(ctx, p, changed)
	require.Error(t, err)
	require.Error(t, broken.ClearFindingTriage(ctx, p, org.ID, graph, fp))
	rows, err = st.ListFindingTriage(ctx, org.ID, graph, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, rows[0].Reason, "Input is a constant from config")

	require.NoError(t, svc.ClearFindingTriage(ctx, accounts.Principal{User: editor, Via: "session"}, org.ID, graph, fp))
	assert.True(t, errors.Is(svc.ClearFindingTriage(ctx, p, org.ID, graph, fp), accounts.ErrNotFound))
	events, err = st.ListEvents(ctx, accounts.EventFilter{OrgID: org.ID, Action: "finding.triage.clear"})
	require.NoError(t, err)
	assert.Len(t, events, 1)

	// The bound holds; replacing an existing decision still works when full.
	for i := range accounts.MaxFindingTriage {
		row := base
		row.Fingerprint = fmt.Sprintf("%064x", i)
		row.ActorID, row.CreatedAt = owner.ID, time.Now()
		require.NoError(t, st.PutFindingTriage(ctx, row))
	}
	over := base
	over.Fingerprint = strings.Repeat("f", 64)
	over.ActorID, over.CreatedAt = owner.ID, time.Now()
	assert.True(t, errors.Is(st.PutFindingTriage(ctx, over), accounts.ErrAccessLimit))
	again := base
	again.Fingerprint = fmt.Sprintf("%064x", 0)
	again.ActorID, again.CreatedAt = owner.ID, time.Now()
	require.NoError(t, st.PutFindingTriage(ctx, again))
	rows, err = st.ListFindingTriage(ctx, org.ID, graph, 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	_, err = st.RemoveRepository(ctx, org.ID, graph)
	require.NoError(t, err)
	rows, err = st.ListFindingTriage(ctx, org.ID, graph, 10)
	require.NoError(t, err)
	assert.Len(t, rows, 0)
}
