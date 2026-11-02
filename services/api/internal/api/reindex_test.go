package api

import (
	"strconv"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestReindexOutdated: a graph from an older IndexVersion is re-indexed as
// the schedule's member, audited before the start; a failed start is audited
// too and waits reindexRetry. Current graphs and paused schedules are left alone.
func TestReindexOutdated(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := t.Context()
	f.client.auth = ""
	require.Equal(t, f.client.do("POST", "/api/v1/auth/login", `{"email":"runtime@example.com","password":"`+pw+`"}`, nil), 200)
	path := "/api/v1/repos/" + storetest.Repo + "/schedule"
	require.Equal(t, f.client.do("PUT", path, `{"interval_minutes":60}`, nil, runtimeHeaders("thread-a")...), 200)
	repo, err := f.store.Node(ctx, model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo))
	require.NoError(t, err)
	require.Equal(t, model.IndexVersionOf(repo), 0)

	events := func() []accounts.Event {
		evs, err := f.accounts.St.ListEvents(ctx, accounts.EventFilter{OrgID: f.org.ID, Action: "repo.reindex", Limit: 10})
		require.NoError(t, err)
		return evs
	}
	now := time.Now()

	current := repo
	current.Meta = `{"index_version":` + strconv.Itoa(model.IndexVersion) + `}`
	f.server.reindexOutdated(ctx, []model.Node{current}, now, f.server.logger)
	assert.Equal(t, len(events()), 0, "a current graph is not re-indexed")

	require.Equal(t, f.client.do("PUT", path, `{"interval_minutes":60,"paused":true}`, nil, runtimeHeaders("thread-a")...), 200)
	f.server.reindexOutdated(ctx, []model.Node{repo}, now, f.server.logger)
	assert.Equal(t, len(events()), 0, "a paused schedule is not re-indexed")

	require.Equal(t, f.client.do("PUT", path, `{"interval_minutes":60}`, nil, runtimeHeaders("thread-a")...), 200)
	f.server.reindexOutdated(ctx, []model.Node{repo}, now, f.server.logger)
	evs := events()
	// The fixture's repository has no source on disk, so the start fails after the audit.
	require.Len(t, evs, 2)
	outcomes := map[string]accounts.Event{}
	for _, e := range evs {
		outcomes[e.Outcome] = e
	}
	ok := outcomes[accounts.Success]
	assert.Equal(t, ok.UserID, f.user.ID)
	assert.Equal(t, ok.Via, "automation")
	assert.Equal(t, ok.Target, storetest.Repo)
	assert.Equal(t, ok.Detail, "analysis version 0 is older than "+strconv.Itoa(model.IndexVersion))
	assert.Equal(t, outcomes[accounts.Failure].Target, storetest.Repo)

	f.server.reindexOutdated(ctx, []model.Node{repo}, now.Add(time.Hour), f.server.logger)
	assert.Equal(t, len(events()), 2, "no new try before reindexRetry")
	f.server.reindexOutdated(ctx, []model.Node{repo}, now.Add(reindexRetry), f.server.logger)
	assert.Equal(t, len(events()), 4, "tried again after reindexRetry")
}

func TestReindexerForgetsCurrentRepositories(t *testing.T) {
	var r reindexer
	now := time.Now()
	assert.True(t, r.try("a", now))
	assert.False(t, r.try("a", now))
	r.forget(map[string]bool{})
	assert.True(t, r.try("a", now), "a repository that was current again starts over")
}
