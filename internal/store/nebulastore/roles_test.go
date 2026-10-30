package nebulastore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"

	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// checkOnly is how a worker or the MCP service uses the store: it checks the
// schema and never changes it.
type checkOnly struct{ *Store }

func (c checkOnly) EnsureSchema(ctx context.Context) error { return c.CheckSchema(ctx) }

// TestRoles runs the deployment's split of NebulaGraph roles: root creates
// the space and the users, a DBA owns the schema, and a USER (workers, MCP)
// and a GUEST can change no schema while the USER passes the whole store
// contract.
func TestRoles(t *testing.T) {
	addr := os.Getenv("CODESYNAPSE_TEST_NEBULA_ADDR")
	if addr == "" {
		t.Skip("CODESYNAPSE_TEST_NEBULA_ADDR not set")
	}
	user, pass := os.Getenv("CODESYNAPSE_TEST_NEBULA_USER"), os.Getenv("CODESYNAPSE_TEST_NEBULA_PASSWORD")
	if user == "" {
		user, pass = "root", "nebula"
	}
	if user != "root" {
		t.Skip("creating users needs root")
	}
	ctx := context.Background()
	addrs := strings.Split(addr, ",")
	const space = "cs_roles_test"
	cfg := func(user, pass, space string) Config {
		return Config{Addrs: addrs, Space: space, User: user, Password: pass, Partitions: 3, Vectors: vector.NewFlat(), SchemaWait: 2 * time.Minute}
	}

	root, err := New(ctx, cfg(user, pass, space))
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close(ctx) })
	asRoot := func(stmt string) error {
		return root.admin(ctx, func(sess *nebula.Session) error { rs, err := sess.Execute(stmt); return check(rs, err) })
	}
	// What the init step does: the space and one user per role.
	require.NoError(t, asRoot("CREATE SPACE IF NOT EXISTS "+quote(space)+" (partition_num = 3, replica_factor = 1, vid_type = FIXED_STRING(40))"))
	passwords := map[string]string{}
	for _, role := range []string{"DBA", "USER", "GUEST"} {
		name := "cs_t_" + strings.ToLower(role)
		passwords[role] = randomPassword(t)
		require.NoError(t, asRoot(fmt.Sprintf("CREATE USER IF NOT EXISTS %s WITH PASSWORD %q", name, passwords[role])))
		require.NoError(t, asRoot(fmt.Sprintf("ALTER USER %s WITH PASSWORD %q", name, passwords[role])))
		// The space reaches graphd on its next heartbeat.
		require.NoError(t, root.until(ctx, time.Now().Add(time.Minute), "grant", func() error {
			return asRoot(fmt.Sprintf("GRANT ROLE %s ON %s TO %s", role, quote(space), name))
		}))
	}
	open := func(role, space string) *Store {
		t.Helper()
		var s *Store
		require.NoError(t, root.until(ctx, time.Now().Add(time.Minute), "login as "+role, func() (err error) {
			s, err = New(ctx, cfg("cs_t_"+strings.ToLower(role), passwords[role], space))
			return err
		}))
		t.Cleanup(func() { _ = s.Close(ctx) })
		return s
	}

	// Before the owner ran, a USER sees a missing schema, and may not make it.
	usr := open("USER", space)
	err = usr.CheckSchema(ctx)
	assert.True(t, errors.Is(err, store.ErrSchemaMissing), "an empty space has no schema: %v", err)
	err = usr.EnsureSchema(ctx)
	require.Error(t, err)
	assert.True(t, isPermission(err), "a USER may not create a tag: %v", err)
	assert.Contains(t, err.Error(), "DBA")

	// A space that does not exist is missing too; only root may create it.
	absent := open("USER", "cs_roles_absent")
	assert.True(t, errors.Is(absent.CheckSchema(ctx), store.ErrSchemaMissing))
	err = absent.EnsureSchema(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "may not create it")
	dbaAbsent := open("DBA", "cs_roles_absent")
	assert.True(t, isPermission(dbaAbsent.EnsureSchema(ctx)), "a DBA may not create a space")

	// The DBA owns the schema of the space root made.
	dba := open("DBA", space)
	require.NoError(t, dba.EnsureSchema(ctx))
	require.NoError(t, dba.EnsureSchema(ctx), "idempotent for the owner too")
	require.NoError(t, usr.CheckSchema(ctx))

	// The GUEST reads and is refused every change.
	guest := open("GUEST", space)
	require.NoError(t, guest.CheckSchema(ctx))
	assert.True(t, isPermission(guest.EnsureSchema(ctx)), "a GUEST may not change the schema")
	err = guest.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture())
	require.Error(t, err)
	_, err = guest.Repos(ctx)
	require.NoError(t, err, "a GUEST reads")

	// The USER may not change the schema but runs everything else.
	for _, ddl := range []string{
		"CREATE TAG IF NOT EXISTS `cs_t_extra` (`a` string NULL)",
		"ALTER TAG `node` ADD (`cs_t_extra` string NULL)",
		"CREATE TAG INDEX IF NOT EXISTS `cs_t_extra_ix` ON `node` (`repo`(8))",
		"DROP TAG INDEX IF EXISTS `node_by_repo`",
		"DROP TAG IF EXISTS `review`",
	} {
		_, err := usr.write(ctx, ddl, nil)
		assert.True(t, isPermission(err), "%s: %v", ddl, err)
	}
	storetest.Run(t, checkOnly{usr})
}

func randomPassword(t *testing.T) string {
	b := make([]byte, 12)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}
