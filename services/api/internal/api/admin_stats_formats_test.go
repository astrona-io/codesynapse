package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIngestFormats(t *testing.T) {
	ctx := context.Background()
	_, accSt, st, ada, signup := statsServer(t)
	member, ua, orgA := signup("a@example.com")
	_, ub, orgB := signup("b@example.com")
	_, uc, orgC := signup("c@example.com")
	_, ud, orgD := signup("d@example.com")
	now := time.Now().UTC()
	attach := func(org accounts.Org, user accounts.User, graph, access string) {
		require.NoError(t, accSt.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: graph, Remote: "https://example.com/" + graph + ".git",
			Visibility: accounts.Private, AddedAt: now, AddedBy: user.ID, Access: access}))
	}
	snap := func(graph string, formats map[string]int) {
		require.NoError(t, st.AddSnapshot(ctx, graph, history.Snapshot{At: now.Add(-time.Hour).Format(time.RFC3339), Formats: formats}))
	}
	// shared-secret is held by A and B and counts once; Go is in four
	// organisations, every other format in fewer than three. A's lost entry
	// is left out, and D's second graph has no snapshot yet.
	attach(orgA, ua, "acme/shared-secret", "")
	attach(orgB, ub, "acme/shared-secret", "")
	attach(orgC, uc, "acme/c-secret", "")
	attach(orgD, ud, "acme/d-secret", "")
	attach(orgD, ud, "acme/unindexed-secret", "")
	attach(orgA, ua, "acme/lost-secret", accounts.AccessLost)
	snap("acme/shared-secret", map[string]int{"go": 10, "python": 3})
	snap("acme/c-secret", map[string]int{"go": 5, "yaml": 2})
	snap("acme/d-secret", map[string]int{"go": 1, "markdown": 4})
	snap("acme/lost-secret", map[string]int{"go": 100, "rust": 99})

	var body string
	require.Equal(t, ada.do("GET", "/api/v1/admin/stats/ingest?range=24h", "", &body), 200)
	for _, secret := range []string{"secret", "example.com"} {
		assert.False(t, strings.Contains(body, secret), "leaks "+secret)
	}
	var s IngestSection
	require.Equal(t, ada.do("GET", "/api/v1/admin/stats/ingest?range=24h", "", &s), 200)
	assert.Equal(t, s.Formats, []PlatformFormatCount{{Name: "go", Files: 16}, {Name: "other", Files: 9}},
		"each graph once, small groups as other, the lost entry left out")
	assert.Equal(t, s.FormatsUnknown, 1)

	var one IngestSection
	require.Equal(t, ada.do("GET", "/api/v1/admin/stats/ingest?range=24h&org="+orgB.ID, "", &one), 200)
	assert.True(t, one.Formats == nil, "no platform breakdown for one organisation")
	assert.Equal(t, member.do("GET", "/api/v1/admin/stats/ingest?range=24h", "", nil), 403)

	var js string
	require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=ingest&range=24h&format=json", "", &js), 200)
	assert.Contains(t, js, `"formats":[{"name":"go","files":16},{"name":"other","files":9}],"formats_unknown":1`)
}
