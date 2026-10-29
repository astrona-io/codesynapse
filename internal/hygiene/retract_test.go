package hygiene

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const retractingGoMod = `module example.com/Mod

go 1.22

// Published with a broken build.
retract v1.0.0

retract (
	[v1.1.0, v1.1.5] // Data race in the cache.
	v1.2.0-rc.1
	v1.3.0
)
`

func TestRetractedBy(t *testing.T) {
	f, err := modfile.ParseLax("go.mod", []byte(retractingGoMod), nil)
	require.NoError(t, err)
	require.Len(t, f.Retract, 4)
	for _, tc := range []struct {
		version string
		want    string
		ok      bool
	}{
		{"v1.0.0", "Published with a broken build.", true},
		{"v1.1.0", "Data race in the cache.", true},
		{"v1.1.3", "Data race in the cache.", true},
		{"v1.1.5", "Data race in the cache.", true},
		{"v1.1.6", "", false},
		{"v1.0.1", "", false},
		{"v1.2.0-rc.1", "retracted", true},
		{"v1.2.0", "", false},
		{"v1.3.0", "retracted", true},
		{"v1.1.2-0.20240101120000-abcdef123456", "Data race in the cache.", true},
		{"1.0.0", "", false}, // not a Go version
		{"", "", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			got, ok := RetractedBy(f.Retract, tc.version)
			assert.Equal(t, ok, tc.ok)
			assert.Equal(t, got, tc.want)
		})
	}
	long := []*modfile.Retract{{VersionInterval: modfile.VersionInterval{Low: "v1.0.0", High: "v1.0.0"}, Rationale: strings.Repeat("x", 500)}}
	got, _ := RetractedBy(long, "v1.0.0")
	assert.True(t, len(got) <= maxRationale+3, "the rationale is capped")
}

func TestRetracted(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.EscapedPath())
		mu.Unlock()
		switch r.URL.EscapedPath() {
		case "/example.com/!mod/@latest":
			_, _ = w.Write([]byte(`{"Version":"v1.4.0","Time":"2026-01-01T00:00:00Z"}`))
		case "/example.com/!mod/@v/v1.4.0.mod":
			_, _ = w.Write([]byte(retractingGoMod))
		case "/example.com/moved/@latest":
			http.Redirect(w, r, "/example.com/!mod/@latest", http.StatusFound)
		case "/example.com/badversion/@latest":
			_, _ = w.Write([]byte(`{"Version":"../../etc"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	deps := []license.Dep{
		{Ecosystem: license.Go, Name: "example.com/Mod", Version: "v1.1.2"},
		{Ecosystem: license.Go, Name: "example.com/Mod", Version: "v1.4.0"},
		{Ecosystem: license.Go, Name: "example.com/Mod", Version: "v1.0.0"},
		{Ecosystem: license.Go, Name: "example.com/moved", Version: "v1.0.0"},
		{Ecosystem: license.Go, Name: "example.com/badversion", Version: "v1.0.0"},
		{Ecosystem: license.Go, Name: "example.com/unknown", Version: "v1.0.0"},
		{Ecosystem: license.Go, Name: "../escape", Version: "v1.0.0"},
		{Ecosystem: license.NPM, Name: "left-pad", Version: "1.0.0"},
	}
	got := Retracted(context.Background(), &GoProxy{BaseURL: srv.URL}, deps)
	assert.Equal(t, got, map[string]string{
		deps[0].Key(): "Data race in the cache.",
		deps[2].Key(): "Published with a broken build.",
	})
	assert.Len(t, asked, 5, "@latest and one go.mod for Mod, @latest for moved, badversion and unknown; nothing for other ecosystems or invalid paths")
	for _, p := range asked {
		assert.False(t, strings.Contains(p, "left-pad") || strings.Contains(p, "escape"), p)
	}
}
