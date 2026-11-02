package fga

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The contract runs against a real OpenFGA server (`just test-openfga`
// starts a throwaway one): every row of "Rules the model must keep" in
// docs/plans/OPENFGA_PLAN.md that the model decides. Each test gets its own
// store, so tuples never leak between rules.

func contractClient(t *testing.T) *Client {
	t.Helper()
	c, _ := contractStore(t)
	return c
}

// contractStore returns a client on a new store and the store's name.
func contractStore(t *testing.T) (*Client, string) {
	t.Helper()
	url, token := os.Getenv("CODESYNAPSE_TEST_OPENFGA_URL"), os.Getenv("CODESYNAPSE_TEST_OPENFGA_TOKEN")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_OPENFGA_URL not set (just test-openfga)")
	}
	c, err := New(Config{URL: url, Token: token, Timeout: 10 * time.Second})
	require.NoError(t, err)
	var b [6]byte
	_, _ = rand.Read(b[:])
	ctx := context.Background()
	name := "contract-" + hex.EncodeToString(b[:])
	_, err = c.EnsureStore(ctx, name)
	require.NoError(t, err)
	_, err = c.EnsureModel(ctx)
	require.NoError(t, err)
	return c, name
}

func u(name string) string    { return TypeUser + ":" + name }
func org(name string) string  { return TypeOrganisation + ":" + name }
func team(name string) string { return TypeTeam + ":" + name }
func repo(name string) string { return TypeRepository + ":" + name }

// Tuples the API writes, named after the plan's rules.
func member(user, o string) Tuple       { return Tuple{u(user), "member", org(o)} }
func guest(user, o string) Tuple        { return Tuple{u(user), "guest", org(o)} }
func orgRole(user, rel, o string) Tuple { return Tuple{u(user), rel, org(o)} }
func defaultRead(o string) Tuple {
	return Tuple{org(o) + "#member", "default_readers", org(o)}
}
func teamOf(tm, o string) Tuple        { return Tuple{org(o), "organisation", team(tm)} }
func teamMember(user, tm string) Tuple { return Tuple{u(user), "member", team(tm)} }
func orgRepo(r, o string) []Tuple {
	return []Tuple{{org(o), "organisation", repo(r)}, {org(o), "org_space", repo(r)}}
}
func personalRepo(r, o string) Tuple     { return Tuple{org(o), "organisation", repo(r)} }
func direct(user, level, r string) Tuple { return Tuple{u(user), "direct_" + level, repo(r)} }
func teamGrant(tm, level, r string) Tuple {
	return Tuple{team(tm) + "#member", "team_" + level, repo(r)}
}

func write(t *testing.T, c *Client, tuples ...Tuple) {
	t.Helper()
	require.NoError(t, c.WriteTuples(context.Background(), tuples))
}

func remove(t *testing.T, c *Client, tuples ...Tuple) {
	t.Helper()
	require.NoError(t, c.DeleteTuples(context.Background(), tuples))
}

// levels asserts what user may do on r: "full", "write", "read" or "".
func levels(t *testing.T, c *Client, user, r, want string) {
	t.Helper()
	checks := []Tuple{{u(user), "read", repo(r)}, {u(user), "write", repo(r)}, {u(user), "full", repo(r)}}
	got, err := c.BatchCheck(context.Background(), checks)
	require.NoError(t, err)
	var exp []bool
	switch want {
	case "full":
		exp = []bool{true, true, true}
	case "write":
		exp = []bool{true, true, false}
	case "read":
		exp = []bool{true, false, false}
	default:
		exp = []bool{false, false, false}
	}
	assert.Equal(t, got, exp, "%s on %s (read, write, full)", user, r)
	// Check and BatchCheck must agree.
	for i, ch := range checks {
		ok, err := c.Check(context.Background(), ch)
		require.NoError(t, err)
		assert.Equal(t, ok, exp[i], "Check %s %s %s", user, ch.Relation, r)
	}
}

func TestContractEnsureIsIdempotent(t *testing.T) {
	c, name := contractStore(t)
	ctx := context.Background()
	store, model := c.StoreID(), c.ModelID()

	// A second process finds the same store and model instead of making new ones.
	other, err := New(Config{URL: os.Getenv("CODESYNAPSE_TEST_OPENFGA_URL"), Token: os.Getenv("CODESYNAPSE_TEST_OPENFGA_TOKEN")})
	require.NoError(t, err)
	gotStore, err := other.EnsureStore(ctx, name)
	require.NoError(t, err)
	gotModel, err := other.EnsureModel(ctx)
	require.NoError(t, err)
	assert.Equal(t, gotStore, store)
	assert.Equal(t, gotModel, model)
}

func TestContractDirectGrantOverridesTeams(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, member("alice", "o"), member("bob", "o"), teamOf("t", "o"),
		teamMember("alice", "t"), teamMember("bob", "t"), teamGrant("t", "full", "r"),
		defaultRead("o"))
	levels(t, c, "bob", "r", "full")

	// A weaker direct grant replaces the team's full grant.
	write(t, c, direct("alice", "read", "r"))
	levels(t, c, "alice", "r", "read")
	write(t, c, direct("bob", "write", "r"))
	levels(t, c, "bob", "r", "write")

	// Removing the direct grant brings the team grant back.
	remove(t, c, direct("alice", "read", "r"))
	levels(t, c, "alice", "r", "full")
}

func TestContractDenyDenies(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, member("carol", "o"), teamOf("t", "o"), teamMember("carol", "t"),
		teamGrant("t", "full", "r"), defaultRead("o"))
	levels(t, c, "carol", "r", "full")
	write(t, c, direct("carol", "deny", "r"))
	levels(t, c, "carol", "r", "")
}

func TestContractStrongestTeamGrantWins(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, member("dave", "o"), teamOf("t1", "o"), teamOf("t2", "o"),
		teamMember("dave", "t1"), teamMember("dave", "t2"),
		teamGrant("t1", "read", "r"), teamGrant("t2", "write", "r"))
	levels(t, c, "dave", "r", "write")
	// A team maintainer (team full) receives the team's grants too.
	write(t, c, member("mia", "o"), Tuple{u("mia"), "full", team("t2")})
	levels(t, c, "mia", "r", "write")
}

func TestContractOrganisationDefault(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, member("erin", "o"))
	// NONE: no default_readers tuple, no access.
	levels(t, c, "erin", "r", "")
	// READ_ONLY gives read and nothing more.
	write(t, c, defaultRead("o"))
	levels(t, c, "erin", "r", "read")
	// Back to NONE.
	remove(t, c, defaultRead("o"))
	levels(t, c, "erin", "r", "")
}

func TestContractGuestsGetNoDefault(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, guest("frank", "o"), defaultRead("o"))
	levels(t, c, "frank", "r", "")
	// An explicit grant still works for a guest.
	write(t, c, direct("frank", "read", "r"))
	levels(t, c, "frank", "r", "read")
}

func TestContractPersonalReposGetNoDefault(t *testing.T) {
	c := contractClient(t)
	write(t, c, personalRepo("p", "o"), member("erin", "o"), member("owner", "o"),
		defaultRead("o"), direct("owner", "full", "p"))
	levels(t, c, "erin", "p", "")
	levels(t, c, "owner", "p", "full")
	// Team grants still apply to a personal entry its owner shared.
	write(t, c, teamOf("t", "o"), teamMember("erin", "t"), teamGrant("t", "read", "p"))
	levels(t, c, "erin", "p", "read")
}

func TestContractOnlyActiveMembers(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, direct("gina", "full", "r"), teamOf("t", "o"), teamMember("hal", "t"),
		teamGrant("t", "write", "r"), defaultRead("o"))
	// Grants without a membership give nothing.
	levels(t, c, "gina", "r", "")
	levels(t, c, "hal", "r", "")
	write(t, c, member("gina", "o"), member("hal", "o"))
	levels(t, c, "gina", "r", "full")
	levels(t, c, "hal", "r", "write")
	// Membership ended or account suspended: the member tuple goes.
	remove(t, c, member("gina", "o"), member("hal", "o"))
	levels(t, c, "gina", "r", "")
	levels(t, c, "hal", "r", "")
	// Membership of another organisation does not count.
	write(t, c, member("gina", "other"))
	levels(t, c, "gina", "r", "")
}

func TestContractOrganisationRolesGiveNoCodeAccess(t *testing.T) {
	c := contractClient(t)
	write(t, c, orgRepo("r", "o")...)
	write(t, c, member("olga", "o"), orgRole("olga", "owner", "o"), orgRole("olga", "full", "o"),
		orgRole("olga", "write", "o"), orgRole("olga", "read", "o"))
	levels(t, c, "olga", "r", "")
	// The organisation levels themselves resolve as the plan says.
	for _, rel := range []string{"owner", "full", "write", "read", "active"} {
		ok, err := c.Check(context.Background(), Tuple{u("olga"), rel, org("o")})
		require.NoError(t, err)
		assert.True(t, ok, rel)
	}
	// Someone never written as a tuple (a platform admin, root) has nothing.
	levels(t, c, "platform-admin", "r", "")
}

func TestContractLostEntriesAreInvisible(t *testing.T) {
	c := contractClient(t)
	ctx := context.Background()
	entry := append(orgRepo("r", "o"), direct("ivan", "full", "r"), teamGrant("t", "read", "r"))
	write(t, c, entry...)
	write(t, c, orgRepo("s", "o")...)
	write(t, c, member("ivan", "o"), member("jo", "o"), teamOf("t", "o"), teamMember("jo", "t"), defaultRead("o"))
	levels(t, c, "ivan", "r", "full")
	levels(t, c, "jo", "r", "read")

	list, err := c.ListObjects(ctx, u("ivan"), "read", TypeRepository)
	require.NoError(t, err)
	slices.Sort(list)
	assert.Equal(t, list, []string{repo("r"), repo("s")})

	// Marked lost: every tuple whose object is the entry goes.
	remove(t, c, entry...)
	levels(t, c, "ivan", "r", "")
	levels(t, c, "jo", "r", "")
	list, err = c.ListObjects(ctx, u("ivan"), "read", TypeRepository)
	require.NoError(t, err)
	assert.Equal(t, list, []string{repo("s")})

	// Restored: written again.
	write(t, c, entry...)
	levels(t, c, "ivan", "r", "full")
	levels(t, c, "jo", "r", "read")
}

func TestContractWritesAreIdempotent(t *testing.T) {
	c := contractClient(t)
	tuples := append(orgRepo("r", "o"), member("kim", "o"), direct("kim", "write", "r"))
	write(t, c, tuples...)
	// The outbox may replay a batch; duplicates and missing deletes are no error.
	write(t, c, tuples...)
	levels(t, c, "kim", "r", "write")
	remove(t, c, direct("kim", "write", "r"))
	remove(t, c, direct("kim", "write", "r"))
	levels(t, c, "kim", "r", "")
}
