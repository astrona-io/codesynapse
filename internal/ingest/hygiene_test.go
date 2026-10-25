package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
