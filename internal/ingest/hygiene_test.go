package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/astrona-io/codesynapse/internal/hygiene"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestDependencyHygiene: with deps.dev lookups on, Module nodes say whether
// their version is deprecated and how far behind the latest release it is.
func TestDependencyHygiene(t *testing.T) {
	root := t.TempDir()
	lock := `{"name":"app","lockfileVersion":3,"packages":{
		"":{"name":"app","dependencies":{"request":"^2.88.0"}},
		"node_modules/request":{"version":"2.88.0","license":"Apache-2.0"}}}`
	require.NoError(t, os.WriteFile(filepath.Join(root, "package-lock.json"), []byte(lock), 0o600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/systems/NPM/packages/request" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"versions":[
			{"versionKey":{"version":"2.88.0"},"publishedAt":"2018-08-10T00:00:00Z","isDeprecated":true,"deprecatedReason":"request has been deprecated"},
			{"versionKey":{"version":"2.88.2"},"publishedAt":"2020-02-11T00:00:00Z","isDefault":true,"isDeprecated":true}]}`))
	}))
	defer srv.Close()

	for _, online := range []bool{false, true} {
		g, err := Run(context.Background(), Options{Repo: "r", Root: root, LicenseLookup: online, DepsDev: &license.DepsDev{BaseURL: srv.URL}})
		require.NoError(t, err)
		n, ok := g.Node(model.NodeID("r", model.KindModule, "npm:request"))
		require.True(t, ok)
		if !online {
			assert.Equal(t, n.Latest+n.Deprecated, "", "nothing is asked without the lookup")
			continue
		}
		assert.Equal(t, n.Latest, "2.88.2")
		assert.Equal(t, n.Deprecated, "request has been deprecated")
		assert.Equal(t, n.Behind, 1)
	}
}

// TestGoRetractions: with lookups on, a Go module whose version the
// retract directives of its latest go.mod cover is marked retracted.
func TestGoRetractions(t *testing.T) {
	root := t.TempDir()
	mod := "module example.com/app\n\ngo 1.22\n\nrequire (\n\texample.com/lib v1.1.0\n\texample.com/ok v1.0.0\n)\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod), 0o600))
	var mu sync.Mutex
	var asked []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/example.com/lib/@latest":
			_, _ = w.Write([]byte(`{"Version":"v1.2.0"}`))
		case "/example.com/lib/@v/v1.2.0.mod":
			_, _ = w.Write([]byte("module example.com/lib\n\nretract [v1.0.0, v1.1.9] // Corrupts the index.\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	depsDev := httptest.NewServer(http.NotFoundHandler())
	defer depsDev.Close()

	for _, online := range []bool{false, true} {
		g, err := Run(context.Background(), Options{Repo: "r", Root: root, LicenseLookup: online,
			DepsDev: &license.DepsDev{BaseURL: depsDev.URL}, GoProxy: &hygiene.GoProxy{BaseURL: proxy.URL}})
		require.NoError(t, err)
		lib, ok := g.Node(model.NodeID("r", model.KindModule, "example.com/lib"))
		require.True(t, ok)
		fine, ok := g.Node(model.NodeID("r", model.KindModule, "example.com/ok"))
		require.True(t, ok)
		assert.Equal(t, fine.Retracted, "")
		mu.Lock()
		n := len(asked)
		mu.Unlock()
		if !online {
			assert.Equal(t, lib.Retracted, "")
			assert.Equal(t, n, 0, "nothing is asked without the lookup")
			continue
		}
		assert.Equal(t, lib.Retracted, "Corrupts the index.")
	}
}
