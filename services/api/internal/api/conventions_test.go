package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestConventions(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g := storetest.Fixture()
	root, ok := g.Node(model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo))
	require.True(t, ok)
	root.Meta = `{"go_module":"example.com/app","conventions":["errors: fmt.Errorf (3 calls) and errors.New (1)"]}`
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var got struct {
		Repo        string   `json:"repo"`
		Conventions []string `json:"conventions"`
	}
	require.Equal(t, get(t, ts, "/api/v1/repos/"+storetest.Repo+"/conventions", &got), 200)
	assert.Equal(t, got.Conventions, []string{"errors: fmt.Errorf (3 calls) and errors.New (1)"})
	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/conventions", nil), 404)

	// A graph indexed before conventions existed answers an empty list.
	plain, _ := newTestServer(t)
	var none struct {
		Conventions []string `json:"conventions"`
	}
	require.Equal(t, get(t, plain, "/api/v1/repos/"+storetest.Repo+"/conventions", &none), 200)
	assert.Equal(t, none.Conventions, []string{})
}
