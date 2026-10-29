package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestFindingsNotification(t *testing.T) {
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	since := now.Add(-notifyWindow)
	tests := []struct {
		name  string
		snaps []history.Snapshot
		want  string
	}{
		{"one snapshot", []history.Snapshot{{At: at(time.Hour)}}, ""},
		{"nothing new", []history.Snapshot{{At: at(2 * time.Hour), Findings: map[string]int{"high": 2}}, {At: at(time.Hour), Findings: map[string]int{"high": 2, "low": 5}}}, ""},
		{"fewer", []history.Snapshot{{At: at(2 * time.Hour), Findings: map[string]int{"high": 2}}, {At: at(time.Hour), Findings: map[string]int{"high": 1}}}, ""},
		{"new findings", []history.Snapshot{{At: at(2 * time.Hour), Grade: "B", Findings: map[string]int{"high": 1}}, {At: at(time.Hour), Grade: "C", Findings: map[string]int{"high": 2, "critical": 1}}},
			"2 new critical or high findings in r"},
		{"new vulnerability", []history.Snapshot{{At: at(2 * time.Hour)}, {At: at(time.Hour), Vulns: map[string]int{"critical": 1}}}, "1 new serious vulnerability in r"},
		{"too old", []history.Snapshot{{At: at(30 * 24 * time.Hour)}, {At: at(20 * 24 * time.Hour), Findings: map[string]int{"high": 3}}}, ""},
		{"grade fell", []history.Snapshot{{At: at(2 * time.Hour), Grade: "A"}, {At: at(time.Hour), Grade: "C", Findings: map[string]int{"medium": 9}}},
			"Security grade A → C in r"},
		{"grade rose", []history.Snapshot{{At: at(2 * time.Hour), Grade: "C"}, {At: at(time.Hour), Grade: "A"}}, ""},
		{"no checkable grade", []history.Snapshot{{At: at(2 * time.Hour), Grade: "B"}, {At: at(time.Hour), Grade: "N/A"}}, "Security grade B → N/A in r"},
		{"same branch, past another", []history.Snapshot{
			{At: at(3 * time.Hour), Branch: "main", Grade: "B", Findings: map[string]int{"high": 1}},
			{At: at(2 * time.Hour), Branch: "dev", Grade: "F", Findings: map[string]int{"high": 9}},
			{At: at(time.Hour), Branch: "main", Grade: "B", Findings: map[string]int{"high": 2}},
		}, "1 new critical or high finding in r (main)"},
		{"switching branch is not a regression", []history.Snapshot{
			{At: at(2 * time.Hour), Branch: "main", Grade: "A"},
			{At: at(time.Hour), Branch: "dev", Grade: "F", Findings: map[string]int{"critical": 4}},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, ok := findingsNotification("r", tt.snaps, since)
			assert.Equal(t, ok, tt.want != "")
			assert.Equal(t, n.Title, tt.want)
		})
	}
	n, _ := findingsNotification("r", tests[3].snaps, since)
	assert.Equal(t, n.Body, "Security grade B → C")
	assert.Equal(t, n.Link, "/repos/r/security")
	assert.Equal(t, n.Kind, "findings")
	n, _ = findingsNotification("r", tests[6].snaps, since)
	assert.Equal(t, n.Kind, "grade")
	assert.Equal(t, n.Body, "")
}

func feedStore(t *testing.T) *memory.Store {
	t.Helper()
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	now := time.Now().UTC()
	require.NoError(t, st.AddSnapshot(ctx, storetest.Repo, history.Snapshot{At: now.Add(-2 * time.Hour).Format(time.RFC3339), Findings: map[string]int{"high": 1}}))
	require.NoError(t, st.AddSnapshot(ctx, storetest.Repo, history.Snapshot{At: now.Add(-time.Hour).Format(time.RFC3339), Findings: map[string]int{"high": 3}}))
	return st
}

func TestNotificationsFeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := feedStore(t)
	require.NoError(t, st.SaveReview(ctx, reviews.Review{ID: "rv1", Kind: "code", Repo: storetest.Repo, Title: "Tighten the handler", Author: "claude",
		Source: "mcp", CreatedAt: time.Now().UTC().Format(time.RFC3339), UpdatedAt: time.Now().UTC().Format(time.RFC3339)}))
	ts := httptest.NewServer(New(ctx, Config{}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var got Notifications
	require.Equal(t, get(t, ts, "/api/v1/notifications", &got), 200)
	require.Len(t, got.Items, 2)
	assert.Equal(t, got.Items[0].Kind, "review", "newest first")
	assert.Equal(t, got.Items[0].Link, "/repos/"+storetest.Repo+"/reviews?id=rv1")
	assert.Equal(t, got.Items[1].Title, "2 new critical or high findings in "+storetest.Repo)
	assert.Equal(t, got.Unread, 2, "without accounts the browser keeps the mark")
	assert.Equal(t, post(t, ts, "/api/v1/notifications/seen", `{"at":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 204)
	assert.Equal(t, post(t, ts, "/api/v1/notifications/seen", `{"at":"tomorrow"}`), 400)
}

// TestNotificationsPerUser: the seen mark is the caller's own, and another
// organisation's repository never appears.
func TestNotificationsPerUser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := feedStore(t)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 202)
		return c
	}
	_ = signup("ada@example.com")
	bob, dave := signup("bob@example.com"), signup("dave@example.com")
	var acme, ext accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Ext"}`, &ext), 201)
	u, err := acc.St.UserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: acme.ID, Graph: storetest.Repo, AddedBy: u.ID, AddedAt: time.Now()}))

	var got Notifications
	require.Equal(t, bob.do("GET", "/api/v1/notifications", "", &got, OrgHeader, acme.Slug), 200)
	require.Len(t, got.Items, 1)
	assert.Equal(t, got.Unread, 1)
	assert.Equal(t, got.SeenAt, "")
	require.Equal(t, bob.do("POST", "/api/v1/notifications/seen", `{"at":"`+time.Now().UTC().Format(time.RFC3339)+`"}`, nil, OrgHeader, acme.Slug), 204)
	require.Equal(t, bob.do("GET", "/api/v1/notifications", "", &got, OrgHeader, acme.Slug), 200)
	assert.Equal(t, got.Unread, 0, "marked read")
	assert.NotEqual(t, got.SeenAt, "")

	require.Equal(t, dave.do("GET", "/api/v1/notifications", "", &got, OrgHeader, ext.Slug), 200)
	assert.Len(t, got.Items, 0, "another organisation's repository is not in the feed")
}
