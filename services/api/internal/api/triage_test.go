package api

import (
	"encoding/json"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type triageList struct {
	Triage  []accounts.FindingTriage `json:"triage"`
	States  []string                 `json:"states"`
	CanEdit bool                     `json:"can_edit"`
}

func TestFindingTriageEndpoints(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	path := "/api/v1/repos/" + storetest.Repo + "/triage"
	report := "/api/v1/repos/" + storetest.Repo + "/security"
	shell := storetest.ID(model.KindFinding, "app/app.go:27:exec-shell")
	g := storetest.Fixture()
	g.AddNode(model.Node{ID: shell, Repo: storetest.Repo, Kind: model.KindFinding, Name: "Shell", Rule: "exec-shell", Severity: "critical", File: "app/app.go", StartLine: 27, EndLine: 27})
	g.AddEdge(model.Edge{From: storetest.ID(model.KindMethod, "(*app.impl).Do"), To: shell, Kind: model.EdgeHasFinding})
	require.NoError(t, f.store.ReplaceRepo(t.Context(), storetest.Repo, g))

	rt := runtimeHeaders("thread-a")
	var list triageList
	assert.Equal(t, f.client.do("GET", path, "", nil), 403, "triage needs a runtime session")
	require.Equal(t, f.client.do("GET", path, "", &list, rt...), 200)
	assert.True(t, list.CanEdit, "a repository admin's API key may triage")
	assert.Len(t, list.Triage, 0)
	assert.Len(t, list.States, 3)

	body := func(v map[string]any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return string(b)
	}
	cases := []struct {
		name   string
		body   string
		status int
	}{
		{name: "unknown finding", body: body(map[string]any{"finding_id": "missing", "state": "accepted_risk", "reason": "x"}), status: 404},
		{name: "not a finding", body: body(map[string]any{"finding_id": storetest.ID(model.KindFunction, "Run"), "state": "accepted_risk", "reason": "x"}), status: 404},
		{name: "no finding", body: body(map[string]any{"state": "accepted_risk", "reason": "x"}), status: 400},
		{name: "unknown field", body: `{"finding_id":"` + shell + `","state":"accepted_risk","reason":"x","fingerprint":"aa"}`, status: 400},
		{name: "fixed by without reference", body: body(map[string]any{"finding_id": shell, "state": "fixed_by", "reason": "x"}), status: 400},
		{name: "bad state", body: body(map[string]any{"finding_id": shell, "state": "ignored", "reason": "x"}), status: 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, f.client.do("PUT", path, tc.body, nil, rt...), tc.status)
		})
	}

	var saved accounts.FindingTriage
	require.Equal(t, f.client.do("PUT", path, body(map[string]any{"finding_id": shell, "state": "fixed_by", "reference": "412", "reason": "Removed the shell call"}), &saved, rt...), 200)
	assert.Equal(t, saved.Reference, "#412")
	assert.Equal(t, saved.Rule, "exec-shell")
	assert.Len(t, saved.Fingerprint, 64)

	var rep security.Report
	require.Equal(t, f.client.do("GET", report, "", &rep), 200)
	require.Len(t, rep.Suppressed, 1)
	got := rep.Suppressed[0]
	require.True(t, got.Triage != nil)
	assert.Equal(t, got.Triage.State, accounts.TriageFixedBy)
	assert.Equal(t, got.Triage.By, "Runtime")
	assert.Equal(t, got.Fingerprint, saved.Fingerprint)

	// A viewer reads the decisions but cannot change them.
	viewer, _, err := f.accounts.SignUp(t.Context(), "triage-viewer@example.com", "Viewer", pw)
	require.NoError(t, err)
	owner := accounts.Principal{User: f.user, Via: "session"}
	_, err = f.accounts.AddMember(t.Context(), owner, f.org.ID, viewer.Email, accounts.Member)
	require.NoError(t, err)
	_, err = f.accounts.SetRepoGrant(t.Context(), owner, f.org.ID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: viewer.ID}, accounts.Viewer)
	require.NoError(t, err)
	browser := newClient(t, f.client.ts)
	require.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"triage-viewer@example.com","password":"`+pw+`"}`, nil), 200)
	org := []string{"X-CodeSynapse-Org", f.org.Slug}
	var seen triageList
	require.Equal(t, browser.do("GET", path, "", &seen, org...), 200)
	assert.False(t, seen.CanEdit)
	require.Len(t, seen.Triage, 1)
	assert.Equal(t, seen.Triage[0].ActorName, "Runtime")
	assert.Equal(t, browser.do("PUT", path, body(map[string]any{"finding_id": shell, "state": "false_positive", "reason": "x"}), nil, org...), 403)
	assert.Equal(t, browser.do("DELETE", path+"/"+saved.Fingerprint, "", nil, org...), 403)

	require.Equal(t, f.client.do("DELETE", path+"/"+saved.Fingerprint, "", nil, rt...), 204)
	assert.Equal(t, f.client.do("DELETE", path+"/"+saved.Fingerprint, "", nil, rt...), 404)
	var after security.Report
	require.Equal(t, f.client.do("GET", report, "", &after), 200)
	assert.Len(t, after.Suppressed, 0)
	events, err := f.accounts.St.ListEvents(t.Context(), accounts.EventFilter{OrgID: f.org.ID, Action: "finding.triage.clear"})
	require.NoError(t, err)
	assert.Len(t, events, 1)
}
