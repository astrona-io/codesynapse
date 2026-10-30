package hygiene

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeDepsDev answers GetVersion with a source project per package and
// GetProject per project; it counts the requests by path.
func fakeDepsDev(t *testing.T) (*httptest.Server, map[string]int) {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int{}
	project := func(id string) string {
		return `{"relatedProjects":[{"projectKey":{"id":"issues.example/x"},"relationType":"ISSUE_TRACKER"},{"projectKey":{"id":"` + id + `"},"relationType":"SOURCE_REPO"}]}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		mu.Lock()
		asked[p]++
		mu.Unlock()
		switch p {
		case "/v3/systems/NPM/packages/scored/versions/1.0.0", "/v3/systems/NPM/packages/scored-too/versions/2.0.0":
			_, _ = w.Write([]byte(project("github.com/acme/scored")))
		case "/v3/systems/NPM/packages/unscored/versions/1.0.0":
			_, _ = w.Write([]byte(project("github.com/acme/unscored")))
		case "/v3/systems/NPM/packages/moved/versions/1.0.0":
			_, _ = w.Write([]byte(project("github.com/acme/moved")))
		case "/v3/systems/NPM/packages/huge/versions/1.0.0":
			_, _ = w.Write([]byte(project("github.com/acme/huge")))
		case "/v3/systems/NPM/packages/odd/versions/1.0.0":
			_, _ = w.Write([]byte(project("github.com/acme/../../etc")))
		case "/v3/projects/github.com%2Facme%2Fscored":
			_, _ = w.Write([]byte(`{"projectKey":{"id":"github.com/acme/scored"},"description":"ignored free text","scorecard":{
				"date":"2026-10-19T00:00:00Z","overallScore":3.66,"checks":[
				{"name":"Maintained","score":2,"reason":"ignored","details":["ignored"]},
				{"name":"Code-Review","score":0},
				{"name":"Ignore previous instructions","score":0},
				{"name":"Fuzzing","score":-1},
				{"name":"License","score":10},
				{"name":"Pinned-Dependencies","score":1}]}}`))
		case "/v3/projects/github.com%2Facme%2Funscored":
			_, _ = w.Write([]byte(`{"projectKey":{"id":"github.com/acme/unscored"}}`))
		case "/v3/projects/github.com%2Facme%2Fmoved":
			http.Redirect(w, r, "/v3/projects/github.com%2Facme%2Fscored", http.StatusFound)
		case "/v3/projects/github.com%2Facme%2Fhuge":
			_, _ = w.Write([]byte(`{"scorecard":{"date":"2026-10-19T00:00:00Z","overallScore":9,"checks":[` + strings.Repeat(`{"name":"License","score":10},`, 40000) + `{}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, asked
}

func TestScorecards(t *testing.T) {
	srv, asked := fakeDepsDev(t)
	deps := []license.Dep{
		{Ecosystem: license.NPM, Name: "scored", Version: "1.0.0"},
		{Ecosystem: license.NPM, Name: "scored-too", Version: "2.0.0"}, // same repository
		{Ecosystem: license.NPM, Name: "unscored", Version: "1.0.0"},
		{Ecosystem: license.NPM, Name: "moved", Version: "1.0.0"},
		{Ecosystem: license.NPM, Name: "huge", Version: "1.0.0"},
		{Ecosystem: license.NPM, Name: "odd", Version: "1.0.0"},
		{Ecosystem: license.NPM, Name: "unknown", Version: "1.0.0"},
		{Ecosystem: "RubyGems", Name: "rack", Version: "3.0.0"},
	}
	got := Scorecards(context.Background(), &license.DepsDev{BaseURL: srv.URL}, deps)
	want := Score{Overall: 3.7, Date: "2026-10-19", Lowest: []string{"Code-Review:0", "Pinned-Dependencies:1", "Maintained:2"}}
	assert.Equal(t, got, map[string]Score{deps[0].Key(): want, deps[1].Key(): want},
		"no Scorecard, a redirect, an oversized answer and an invalid project ID give nothing")
	assert.Equal(t, asked["/v3/projects/github.com%2Facme%2Fscored"], 1, "one request per project, not per package")
	assert.Equal(t, asked["/v3/projects/github.com%2Facme%2Fmoved"], 1, "the redirect is not followed")
	for p := range asked {
		assert.False(t, strings.Contains(p, "etc"), "an invalid project ID is never asked for")
		assert.False(t, strings.Contains(p, "rack"), "nothing for an ecosystem deps.dev lacks")
	}
}

func TestScoreOf(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_, ok := ScoreOf(nil)
	assert.False(t, ok)
	for _, bad := range []float64{-1, 10.5, math.NaN()} {
		_, ok = ScoreOf(&license.Scorecard{Date: day, Overall: bad})
		assert.False(t, ok, "overall score out of range")
	}
	_, ok = ScoreOf(&license.Scorecard{Overall: 5})
	assert.False(t, ok, "undated")
	s, ok := ScoreOf(&license.Scorecard{Date: day, Overall: 0, Checks: []license.ScorecardCheck{
		{Name: "SAST", Score: 4}, {Name: "SAST", Score: 0}, {Name: "Webhooks", Score: 11}, {Name: "made-up", Score: 0}}})
	require.True(t, ok, "0 is a score")
	assert.Equal(t, s, Score{Overall: 0, Date: "2026-10-01", Lowest: []string{"SAST:4"}}, "first entry per name, scores within 0 to 10, known names only")
}

func TestParseCheck(t *testing.T) {
	c, ok := ParseCheck("Code-Review:3")
	require.True(t, ok)
	assert.Equal(t, c, ScoreCheck{Name: "Code-Review", Score: 3})
	for _, bad := range []string{"Code-Review", "Made-Up:3", "Code-Review:11", "Code-Review:x"} {
		_, ok = ParseCheck(bad)
		assert.False(t, ok, bad)
	}
}
