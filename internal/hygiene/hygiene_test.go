package hygiene

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func day(n int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n) }

func TestStatusOf(t *testing.T) {
	versions := []license.Version{
		{Version: "1.0.0", Published: day(0), Deprecated: true, Reason: "use 2.x, see https://example.com/migrate"},
		{Version: "1.1.0", Published: day(10)},
		{Version: "2.0.0-rc.1", Published: day(20)},
		{Version: "2.0.0", Published: day(30), Default: true},
		{Version: "2.1.0-beta", Published: day(40)},
		{Version: "32.1.3-jre", Published: day(50)},
	}
	s, ok := StatusOf(versions, "1.0.0")
	require.True(t, ok)
	assert.Equal(t, s, Status{Latest: "2.0.0", Deprecated: "use 2.x, see https://example.com/migrate", Behind: 3}, "1.1.0, 2.0.0 and 32.1.3-jre; not the rc or beta")
	s, ok = StatusOf(versions, "2.0.0")
	require.True(t, ok)
	assert.Equal(t, s.Deprecated, "")
	_, ok = StatusOf(versions, "9.9.9")
	assert.False(t, ok)
	s, _ = StatusOf([]license.Version{{Version: "1", Deprecated: true}}, "1")
	assert.Equal(t, s.Deprecated, "deprecated")
	assert.True(t, prerelease.MatchString("v0.0.0-20240101120000-abcdef123456"), "a Go pseudo-version")
	assert.True(t, prerelease.MatchString("2.0b1"))
	assert.False(t, prerelease.MatchString("1.2.3"))
}

func TestCheck(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.EscapedPath())
		mu.Unlock()
		if r.URL.EscapedPath() != "/v3/systems/NPM/packages/%40scope%2Fpkg" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"versions":[
			{"versionKey":{"version":"1.0.0"},"publishedAt":"2026-01-01T00:00:00Z","isDeprecated":true,"deprecatedReason":"moved"},
			{"versionKey":{"version":"2.0.0"},"publishedAt":"2026-02-01T00:00:00Z","isDefault":true}]}`))
	}))
	defer srv.Close()
	deps := []license.Dep{
		{Ecosystem: license.NPM, Name: "@scope/pkg", Version: "1.0.0"},
		{Ecosystem: license.NPM, Name: "@scope/pkg", Version: "2.0.0"},
		{Ecosystem: license.NPM, Name: "unknown", Version: "1.0.0"},
		{Ecosystem: "RubyGems", Name: "rack", Version: "3.0.0"}, // deps.dev has no RubyGems
	}
	got := Check(context.Background(), &license.DepsDev{BaseURL: srv.URL}, deps)
	assert.Equal(t, got, map[string]Status{
		deps[0].Key(): {Latest: "2.0.0", Deprecated: "moved", Behind: 1},
		deps[1].Key(): {Latest: "2.0.0"},
	})
	assert.Len(t, asked, 2, "one request per package; none for an ecosystem deps.dev lacks")
}
