package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func TestAdminPanelQueries(t *testing.T) {
	placeholder := regexp.MustCompile(`\$[A-Za-z_]+`)
	for name, p := range adminPanels {
		t.Run(name, func(t *testing.T) {
			require.True(t, len(p.queries) > 0, "a panel has a query")
			exprs := []string{p.groups}
			for _, q := range p.queries {
				exprs = append(exprs, q.expr)
			}
			for _, e := range exprs {
				for _, m := range placeholder.FindAllString(e, -1) {
					assert.Equal(t, m, "$w", "the only placeholder is the window: "+e)
				}
				assert.False(t, strings.Contains(e, "tenant_id=") || strings.Contains(e, "tenant_id=~"), "tenants are chosen through extra_filters only: "+e)
			}
			if p.groups != "" {
				assert.Contains(t, p.groups, "("+p.queries[0].by+")", "the groups query counts organisations per value of the panel's label")
				assert.False(t, p.byTenant, "per-organisation panels name organisations, not shared values")
			}
		})
	}
	for _, gone := range []string{"tenants", "platform_llm_cost", "platform_tool_latency", "platform_db_latency"} {
		_, ok := seriesPanels[gone]
		assert.False(t, ok, gone+" moved to /admin/stats/series")
	}
}

func line(name string, vs ...float64) SeriesLine {
	l := SeriesLine{Name: name}
	for _, v := range vs {
		l.Values = append(l.Values, &v)
		l.Total += v
	}
	return l
}

func TestFoldSmallGroups(t *testing.T) {
	lines := []SeriesLine{line("search", 1, 2), line("outline", 3, 0), line("rare", 5, 5)}
	used := map[string]int{"search": 3, "outline": 7, "rare": 2}

	keep, small := foldSmallGroups(slices.Clone(lines), used, "count", 2)
	assert.Equal(t, len(keep), 2)
	require.True(t, small != nil, "small groups add up to other")
	assert.Equal(t, small.Name, "other")
	assert.Equal(t, small.Total, 10.0)

	keep, small = foldSmallGroups(slices.Clone(lines), used, "gauge", 2)
	assert.Equal(t, len(keep), 2)
	assert.True(t, small == nil, "levels cannot be summed: small groups are dropped")

	keep, small = foldSmallGroups(slices.Clone(lines), map[string]int{}, "count", 2)
	assert.Equal(t, len(keep), 0, "no value with fewer than three organisations is named")
	assert.Equal(t, small.Total, 16.0)

	// The small groups join the top-N fold's other line.
	many := []SeriesLine{}
	for i := range maxNamedSeries + 2 {
		many = append(many, line(string(rune('a'+i)), float64(100-i)))
	}
	folded := addOther(foldSeries(many, "count", 1), small)
	require.Len(t, folded, maxNamedSeries+1)
	other := folded[len(folded)-1]
	assert.Equal(t, other.Name, "other")
	assert.Equal(t, other.Total, float64(100-maxNamedSeries)+float64(100-maxNamedSeries-1)+16)
	assert.Equal(t, len(addOther([]SeriesLine{line("a", 1)}, small)), 2, "other is added when the top N did not fold")
}

func TestTenantNames(t *testing.T) {
	names := map[string]string{"org1": "Acme"}
	assert.Equal(t, tenantName(names, "org1"), "Acme")
	assert.Equal(t, tenantName(names, "none"), "no organisation")
	assert.Equal(t, tenantName(names, "gone"), deletedOrg)

	lines := []SeriesLine{line("gone1", 1, 2), line("org1", 4, 4), line("gone2", 3, 1)}
	for i := range lines {
		lines[i].Name = tenantName(names, lines[i].Name)
	}
	merged := mergeLines(lines, "count")
	require.Len(t, merged, 2, "deleted organisations are one line")
	assert.Equal(t, merged[0].Name, deletedOrg)
	assert.Equal(t, merged[0].Total, 7.0)
	assert.Equal(t, *merged[0].Values[0], 4.0)
	gauges := mergeLines([]SeriesLine{line(deletedOrg, 1, 5), line(deletedOrg, 3, 2)}, "gauge")
	require.Len(t, gauges, 1)
	assert.Equal(t, *gauges[0].Values[0], 3.0, "levels keep the higher one")
	assert.Equal(t, *gauges[0].Values[1], 5.0)
}

func TestUsageBreakdownFold(t *testing.T) {
	b := newBreakdown()
	for _, org := range []string{"o1", "o2", "o3"} {
		b.add(org, "search", UsageCount{Calls: 2})
	}
	b.add("o1", "rare", UsageCount{Calls: 50})
	b.add("", "anonymous", UsageCount{Calls: 1})
	b.add("o1", "quiet", UsageCount{})
	got := b.fold(true)
	require.Len(t, got, 2)
	assert.Equal(t, got[0], UsageCount{Name: "search", Calls: 6})
	assert.Equal(t, got[1], UsageCount{Name: "other", Calls: 51}, "values of fewer than three organisations are other")
	got = b.fold(false)
	assert.Equal(t, got[0].Name, "rare", "one organisation's own figures are unfolded")
	assert.Equal(t, len(got), 3)

	b = newBreakdown()
	for i := range maxNamedValues + 3 {
		b.add("o1", string(rune('a'+i)), UsageCount{Calls: 100 - i})
	}
	got = b.fold(false)
	require.Len(t, got, maxNamedValues+1)
	assert.Equal(t, got[maxNamedValues].Name, "other")
	assert.Equal(t, got[maxNamedValues].Calls, 100-maxNamedValues+100-maxNamedValues-1+100-maxNamedValues-2)
}

func TestUsageSection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	vm := httptest.NewServer(&fakeVM{})
	t.Cleanup(vm.Close)
	accSt := acctmem.New()
	acc := accounts.NewService(accSt, false)
	bootstrapAccounts(t, acc)
	acc.SignupOpen = true
	st := memory.New()
	srv := New(ctx, Config{Accounts: acc, ServiceToken: "svc-token", Metrics: &telemetry.Querier{URL: vm.URL}}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	signup := func(email, name string) (*client, accounts.User, accounts.Org) {
		c := newClient(t, ts)
		var signed struct {
			User accounts.User
			Org  accounts.Org
		}
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+name+`","password":"`+pw+`"}`, &signed), 201)
		return c, signed.User, signed.Org
	}
	ada, adaUser, adaOrg := signup("ada@example.com", "Ada")
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adaUser.ID, true))
	member, _, orgA := signup("a@example.com", "A")
	_, _, orgB := signup("b@example.com", "B")
	_, _, orgC := signup("c@example.com", "C")
	const gone = "0123456789abcdef01234567"

	today := time.Now().UTC()
	yesterday := usage.Day(today.AddDate(0, 0, -1))
	var buckets []usage.Bucket
	for _, day := range []string{usage.Day(today), yesterday} {
		for _, org := range []string{orgA.ID, orgB.ID, orgC.ID} {
			buckets = append(buckets,
				usage.Bucket{Day: day, Org: org, Tool: usage.Session, Client: "claude-code", Calls: 1},
				usage.Bucket{Day: day, Org: org, Repo: "secret-repo", Tool: "search", Client: "claude-code", Calls: 3, Errors: 1, ResponseTokens: 100})
		}
		buckets = append(buckets,
			usage.Bucket{Day: day, Org: orgA.ID, Repo: "secret-repo", Tool: "get_symbol_context", Client: "Cursor 1.2", Calls: 5, ResponseTokens: 50},
			usage.Bucket{Day: day, Org: orgA.ID, Repo: "private-checkout", Tool: usage.Unindexed, Client: "claude-code", Calls: 1},
			usage.Bucket{Day: day, Org: orgA.ID, Tool: usage.FirstCodeSynapse, Client: "claude-code", Calls: 2},
			usage.Bucket{Day: day, Org: gone, Repo: "secret-repo", Tool: "search", Client: "claude-code", Calls: 7},
			usage.Bucket{Day: day, Repo: "secret-repo", Tool: "search", Client: "claude-code", Calls: 11})
	}
	require.NoError(t, st.AddUsage(ctx, buckets))

	t.Run("short ranges", func(t *testing.T) {
		var u UsageSection
		var body string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/usage?range=24h", "", &body), 200)
		for _, secret := range []string{"secret-repo", "private-checkout", "Cursor 1.2", "share_upper_pct"} {
			assert.False(t, strings.Contains(body, secret), "leaks "+secret)
		}
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/usage?range=24h", "", &u), 200)
		assert.Equal(t, u.From, yesterday, "24h covers yesterday and today")
		assert.Equal(t, u.Totals.Calls, 2*(3*3+5+7+11))
		assert.Equal(t, u.Totals.Sessions, 2*3)
		assert.Equal(t, u.Totals.FirstCodeSynapse, 4)
		require.True(t, u.Totals.UnindexedCheckouts != nil)
		assert.Equal(t, *u.Totals.UnindexedCheckouts, 1, "distinct checkouts, never their names")
		require.Len(t, u.Tools, 2)
		assert.Equal(t, u.Tools[0], UsageCount{Name: "search", Calls: 2 * (3*3 + 7 + 11), Errors: 6}, "with the calls of no organisation")
		assert.Equal(t, u.Tools[1].Name, "other", "a tool of one organisation is other")
		assert.Equal(t, u.Tools[1].Calls, 2*5)
		names := []string{}
		for _, c := range u.Clients {
			names = append(names, c.Name)
		}
		assert.Equal(t, names, []string{"claude", "other"}, "cursor is used by one organisation")

		rows := map[string]OrgUsageRow{}
		for _, r := range u.Rows {
			rows[r.OrgID] = r
		}
		assert.Equal(t, rows[orgA.ID].Calls, 2*8)
		assert.Equal(t, rows[gone].Calls, 14)
		assert.True(t, rows[gone].Deleted && rows[gone].OrgID == gone, "deleted organisations keep their figures")
		assert.Equal(t, rows[gone].Name, deletedOrg)
		_, listed := rows[adaOrg.ID]
		assert.True(t, listed, "every organisation has a row")
		assert.Equal(t, u.Rows[len(u.Rows)-1].Name, deletedOrg, "deleted organisations come last")

		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/usage?range=7d&sort=calls", "", &u), 200)
		assert.Equal(t, u.Rows[0].OrgID, orgA.ID)

		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/usage?range=24h&org="+orgA.ID, "", &u), 200)
		require.Len(t, u.Rows, 1)
		assert.Equal(t, u.Totals.Calls, 2*8)
		toolNames := []string{}
		for _, c := range u.Tools {
			toolNames = append(toolNames, c.Name)
		}
		assert.Equal(t, toolNames, []string{"get_symbol_context", "search"}, "one organisation's figures are unfolded")

		assert.Equal(t, member.do("GET", "/api/v1/admin/stats/usage?range=24h", "", nil), 403)
		var csv string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=usage&range=24h&format=csv", "", &csv), 200)
		assert.True(t, strings.HasPrefix(csv, "slug,name,deleted,sessions,calls"), csv)
		assert.False(t, strings.Contains(csv, "upper") || strings.Contains(csv, "secret-repo"), csv)
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=usage&range=24h&format=json", "", &body), 200)
		assert.False(t, strings.Contains(body, "share_upper_pct"), "exports leave out the upper figure")
	})

	t.Run("long ranges from the rollup", func(t *testing.T) {
		// The server's own loop may have written yesterday before the
		// buckets existed; write the day again, as a rerun would.
		day := accounts.StatDay(today).AddDate(0, 0, -1)
		set := accounts.NewPlatformStatSet(day)
		require.NoError(t, srv.usageStatSource(ctx, day, set))
		require.NoError(t, accSt.PutPlatformStats(ctx, day, set.Rows()))
		var u UsageSection
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/usage?range=90d", "", &u), 200)
		require.True(t, u.Rollup != nil)
		assert.Equal(t, u.Rollup.Written, 1)
		assert.Equal(t, u.Rollup.Unavailable, []string{"unindexed_checkouts"})
		assert.Equal(t, u.To, yesterday, "the rollup ends with yesterday")
		assert.Equal(t, u.Totals.Calls, 3*3+5+7+11, "yesterday only, calls without an organisation included")
		assert.Equal(t, u.Totals.Sessions, 3)
		assert.True(t, u.Totals.UnindexedCheckouts == nil)
		require.True(t, u.Totals.ShareDays != nil)
		require.Len(t, u.Tools, 2)
		assert.Equal(t, u.Tools[0], UsageCount{Name: "search", Calls: 3*3 + 7, Errors: 3})
		assert.Equal(t, u.Tools[1], UsageCount{Name: "other", Calls: 5}, "calls without an organisation have no tool rows")
		rows := map[string]OrgUsageRow{}
		for _, r := range u.Rows {
			rows[r.OrgID] = r
		}
		assert.Equal(t, rows[orgA.ID].Calls, 8)
		assert.Equal(t, rows[orgA.ID].FirstCodeSynapse, 2)
		assert.Equal(t, rows[gone].Calls, 7)

		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/usage?range=365d&org="+orgA.ID, "", &u), 200)
		require.Len(t, u.Rows, 1)
		assert.Equal(t, u.Totals.Calls, 8)
		require.Len(t, u.Tools, 2)
		assert.Equal(t, u.Tools[0], UsageCount{Name: "get_symbol_context", Calls: 5})
		require.Len(t, u.Clients, 2)
		assert.Equal(t, u.Clients[0], UsageCount{Name: "cursor", Calls: 5})
	})
}
