package nebulastore

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// newTestStore connects to the NebulaGraph of CODESYNAPSE_TEST_NEBULA_ADDR
// (just test-nebula) and creates the schema in a throwaway space.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("CODESYNAPSE_TEST_NEBULA_ADDR")
	if addr == "" {
		t.Skip("CODESYNAPSE_TEST_NEBULA_ADDR not set")
	}
	ctx := context.Background()
	user, pass := os.Getenv("CODESYNAPSE_TEST_NEBULA_USER"), os.Getenv("CODESYNAPSE_TEST_NEBULA_PASSWORD")
	if user == "" {
		user, pass = "root", "nebula"
	}
	s, err := New(ctx, Config{Addrs: strings.Split(addr, ","), Space: "cs_test", User: user, Password: pass, Partitions: 3, Vectors: vector.NewFlat()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.EnsureSchema(ctx), "the schema is idempotent")
	return s
}

func TestSchema(t *testing.T) { newTestStore(t) }

func TestCodeGraph(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.DeleteRepo(context.Background(), storetest.Repo))
	storetest.RunCodeGraph(t, s)
}

func TestRecords(t *testing.T) {
	s := newTestStore(t)
	storetest.RunRecords(t, s)
}

// TestStore is the whole contract, the one every store passes.
func TestStore(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, s.DeleteRepo(context.Background(), storetest.Repo))
	storetest.Run(t, s)
}
