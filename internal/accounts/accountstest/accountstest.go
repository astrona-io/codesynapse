// Package accountstest is the contract every accounts.Store follows,
// exercised through accounts.Service so the rules are covered too.
package accountstest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const pw = "correct horse battery staple"

// Run tests st, which must be empty.
func Run(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	require.NoError(t, st.Migrate(ctx))
	require.NoError(t, st.Migrate(ctx), "migrations are idempotent")
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	svc := accounts.NewService(st, false)
	svc.Now = func() time.Time { return now }

	setup, err := svc.Status(ctx)
	require.NoError(t, err)
	assert.True(t, setup, "an empty store needs its first account")

	// Sign-up: the first account is the bootstrap root, which hands over to
	// a standard owner (RunBootstrap covers the details).
	_, err = Bootstrap(ctx, svc)
	require.NoError(t, err)
	_, _, err = svc.SignUp(ctx, "bob@example.com", "Bob", pw)
	assert.True(t, errors.Is(err, accounts.ErrSignupClosed), "sign-up is closed after setup")
	svc.SignupOpen = true
	ada, adaOrg, err := svc.SignUp(ctx, "  Ada@Example.com ", "Ada Lovelace", pw)
	require.NoError(t, err)
	assert.Equal(t, ada.Email, "ada@example.com")
	assert.False(t, ada.PlatformAdmin)
	assert.Equal(t, adaOrg.Slug, "ada-lovelace-s-organisation")
	_, _, err = svc.SignUp(ctx, "ADA@example.com", "Ada again", pw)
	assert.True(t, errors.Is(err, accounts.ErrExists), "emails are unique, case-insensitively")
	_, _, err = svc.SignUp(ctx, "carol@example.com", "Carol", "short")
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "password policy")
	bob, _, err := svc.SignUp(ctx, "bob@example.com", "Bob", pw)
	require.NoError(t, err)
	assert.False(t, bob.PlatformAdmin)

	// Login and sessions.
	_, _, err = svc.Login(ctx, "bob@example.com", "wrong password!!", "1.2.3.4")
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized))
	_, _, err = svc.Login(ctx, "nobody@example.com", pw, "1.2.3.4")
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "unknown emails look the same as wrong passwords")
	tok, _, err := svc.Login(ctx, "BOB@example.com", pw, "1.2.3.4")
	require.NoError(t, err)
	p, err := svc.Authenticate(ctx, tok)
	require.NoError(t, err)
	assert.Equal(t, p.User.ID, bob.ID)
	assert.Equal(t, p.Via, "session")
	now = now.Add(accounts.SessionTTL + time.Second)
	_, err = svc.Authenticate(ctx, tok)
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "sessions expire")
	tok, _, err = svc.Login(ctx, "bob@example.com", pw, "1.2.3.4")
	require.NoError(t, err)
	require.NoError(t, svc.Logout(ctx, tok))
	_, err = svc.Authenticate(ctx, tok)
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized), "logout ends the session")

	// Organisations and roles.
	adaP := accounts.Principal{User: ada, Via: "session"}
	bobP := accounts.Principal{User: bob, Via: "session"}
	team, err := svc.CreateOrg(ctx, bobP, "Platform Team", "")
	require.NoError(t, err)
	_, err = svc.CreateOrg(ctx, bobP, "Other", "platform-team")
	assert.True(t, errors.Is(err, accounts.ErrExists), "explicit slugs must be free")
	dup, err := svc.CreateOrg(ctx, adaP, "Platform team", "")
	require.NoError(t, err)
	assert.Equal(t, dup.Slug, "platform-team-2", "derived slugs get a suffix")
	carol, _, err := svc.SignUp(ctx, "carol@example.com", "Carol", pw)
	require.NoError(t, err)
	carolP := accounts.Principal{User: carol, Via: "session"}
	assert.True(t, errors.Is(svc.Require(ctx, carolP, team.ID, accounts.Viewer), accounts.ErrForbidden), "not a member")
	_, err = svc.AddMember(ctx, bobP, team.ID, "carol@example.com", accounts.Member)
	require.NoError(t, err)
	require.NoError(t, svc.Require(ctx, carolP, team.ID, accounts.Member))
	assert.True(t, errors.Is(svc.Require(ctx, carolP, team.ID, accounts.Admin), accounts.ErrForbidden))
	_, err = svc.AddMember(ctx, carolP, team.ID, "ada@example.com", accounts.Viewer)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "members cannot add members")
	assert.True(t, errors.Is(svc.Require(ctx, adaP, team.ID, accounts.Owner), accounts.ErrForbidden), "platform status grants no organisation membership")
	assert.True(t, errors.Is(svc.RemoveMember(ctx, bobP, team.ID, bob.ID), accounts.ErrLastOwner))
	_, err = svc.AddMember(ctx, bobP, team.ID, "carol@example.com", accounts.Viewer)
	require.NoError(t, err, "changing a role")
	members, err := st.OrgMembers(ctx, team.ID)
	require.NoError(t, err)
	require.Len(t, members, 2)
	assert.Equal(t, members[1].User.Email, "carol@example.com")
	assert.Equal(t, members[1].Role, accounts.Viewer)
	assert.Equal(t, members[1].User.PasswordHash, "", "memberships carry no hashes")
	// Reads report a password without carrying the hash; PasswordHash is the
	// only way to get it.
	for name, find := range map[string]func() (accounts.User, error){
		"id":    func() (accounts.User, error) { return st.UserByID(ctx, bob.ID) },
		"email": func() (accounts.User, error) { return st.UserByEmail(ctx, "bob@example.com") },
	} {
		got, err := find()
		require.NoError(t, err, name)
		assert.Equal(t, got.PasswordHash, "", "by %s: no hash in a user read", name)
		assert.True(t, got.HasPassword, "by %s: bob has a password", name)
	}
	hash, err := st.PasswordHash(ctx, bob.ID)
	require.NoError(t, err)
	assert.True(t, hash != "", "the hash is read apart")
	_, err = st.PasswordHash(ctx, "nobody")
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "unknown user")
	mine, err := st.UserMemberships(ctx, bob.ID)
	require.NoError(t, err)
	assert.Equal(t, len(mine), 2, "Bob's own organisation and the team")

	// API keys.
	key, k, err := svc.CreateKey(ctx, carolP, team.ID, "Claude Code laptop", 0)
	require.NoError(t, err)
	assert.Equal(t, key[:len(k.Prefix)], k.Prefix)
	kp, err := svc.AuthenticateKey(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, kp.OrgID, team.ID)
	assert.Equal(t, kp.Via, "key")
	_, err = svc.RoleIn(ctx, kp, dup.ID)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "a key works in its own organisation only")
	_, _, err = svc.CreateKey(ctx, kp, team.ID, "minted by a key", 0)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "keys do not mint keys")
	keys, err := st.ListAPIKeys(ctx, carol.ID)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.True(t, keys[0].LastUsedAt != nil, "last use is recorded")
	assert.True(t, errors.Is(svc.RevokeKey(ctx, bobP, k.ID), accounts.ErrNotFound), "nobody revokes another user's key")
	require.NoError(t, svc.RevokeKey(ctx, carolP, k.ID))
	_, err = svc.AuthenticateKey(ctx, key)
	assert.True(t, errors.Is(err, accounts.ErrKeyRevoked))
	short, _, err := svc.CreateKey(ctx, carolP, team.ID, "short-lived", time.Hour)
	require.NoError(t, err)
	now = now.Add(2 * time.Hour)
	_, err = svc.AuthenticateKey(ctx, short)
	assert.True(t, errors.Is(err, accounts.ErrKeyRevoked), "keys expire")
	other, _, err := svc.CreateKey(ctx, carolP, team.ID, "removed member", 0)
	require.NoError(t, err)
	require.NoError(t, svc.RemoveMember(ctx, bobP, team.ID, carol.ID))
	_, err = svc.AuthenticateKey(ctx, other)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "removed members' keys stop working")
	_, err = svc.AuthenticateKey(ctx, "csk_not-a-real-key")
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized))

	// Password change ends every session.
	tok, _, err = svc.Login(ctx, "ada@example.com", pw, "5.6.7.8")
	require.NoError(t, err)
	assert.True(t, errors.Is(svc.ChangePassword(ctx, adaP, "not the password", "a brand new passphrase"), accounts.ErrUnauthorized))
	require.NoError(t, svc.ChangePassword(ctx, adaP, pw, "a brand new passphrase"))
	_, err = svc.Authenticate(ctx, tok)
	assert.True(t, errors.Is(err, accounts.ErrUnauthorized))
	_, _, err = svc.Login(ctx, "ada@example.com", "a brand new passphrase", "5.6.7.8")
	require.NoError(t, err)

	// Rate limiting: five tries per email per minute.
	for range 5 {
		_, _, _ = svc.Login(ctx, "carol@example.com", "wrong password!!", "9.9.9.9") //nolint:errcheck // counting attempts
	}
	_, _, err = svc.Login(ctx, "carol@example.com", pw, "9.9.9.9")
	assert.True(t, errors.Is(err, accounts.ErrRateLimited))
	now = now.Add(time.Minute)
	_, _, err = svc.Login(ctx, "carol@example.com", pw, "9.9.9.9")
	require.NoError(t, err, "the limit resets")

	// Audit log: every security event, with request details, hash-chained.
	events, err := st.ListEvents(ctx, accounts.EventFilter{OrgID: team.ID, Limit: 50})
	require.NoError(t, err)
	actions := map[string]bool{}
	for _, e := range events {
		actions[e.Action] = true
		assert.Equal(t, e.OrgID, team.ID)
	}
	for _, a := range []string{"org.create", "member.set", "member.remove", "key.create"} {
		assert.True(t, actions[a], a)
	}
	all, err := st.ListEvents(ctx, accounts.EventFilter{Limit: 1000, Ascending: true})
	require.NoError(t, err)
	failed := 0
	for _, e := range all {
		if e.Action == "user.login" && e.Outcome == accounts.Failure {
			failed++
		}
	}
	assert.True(t, failed >= 3, "wrong password, unknown email and rate limit are all logged")
	ctxReq := accounts.WithRequestInfo(ctx, accounts.RequestInfo{RequestID: "req-1", IP: "10.0.0.7", UserAgent: "curl/8"})
	ev, err := svc.Record(ctxReq, accounts.Event{UserID: bob.ID, Via: "session", OrgID: team.ID, Action: "repo.delete", Target: "api", Outcome: accounts.Success, Status: 204})
	require.NoError(t, err)
	assert.Equal(t, ev.IP, "10.0.0.7")
	assert.Equal(t, ev.RequestID, "req-1")
	assert.Equal(t, ev.PrevHash, all[len(all)-1].Hash, "entries chain onto the previous one")
	// MCP requests carry their details; they join the hash only when present.
	long := strings.Repeat("x", accounts.MaxMCPArgValue+50)
	mcpReq := &accounts.MCPRequest{Method: "tools/call", Name: "get_symbol_context", Repo: "acme/api", SessionID: "chat-1", TransportID: "t-1",
		Client: "claude-code 2.1", DurationMS: 12, Bytes: 2048, ArgsDigest: "sha256:abc", Args: map[string]string{"repo": "acme/api", "query": long, "bad\x00key": "v"}}
	mev, err := svc.Record(ctxReq, accounts.Event{UserID: bob.ID, Via: "key", KeyID: "key-1", OrgID: team.ID, Action: "mcp.tools.call", Target: "get_symbol_context", MCP: mcpReq})
	require.NoError(t, err)
	_, err = svc.Record(ctxReq, accounts.Event{UserID: bob.ID, Via: "key", OrgID: team.ID, Action: "mcp.tools.call", Target: "search",
		MCP: &accounts.MCPRequest{Method: "tools/call", Name: "search", Repo: "acme/web", SessionID: "chat-2"}})
	require.NoError(t, err)
	got, err := st.ListEvents(ctx, accounts.EventFilter{Session: "chat-1", Limit: 10})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.True(t, got[0].MCP != nil, "MCP details round-trip")
	assert.Equal(t, got[0].Hash, mev.Hash)
	assert.Equal(t, got[0].MCP.Name, "get_symbol_context")
	assert.Equal(t, len([]rune(got[0].MCP.Args["query"])), accounts.MaxMCPArgValue, "argument values are capped")
	assert.Equal(t, got[0].MCP.Args["bad key"], "v", "control characters are removed from keys")
	for name, f := range map[string]accounts.EventFilter{"tool": {Tool: "search"}, "repo": {Repo: "acme/web"}} {
		f.Limit = 10
		got, err := st.ListEvents(ctx, f)
		require.NoError(t, err)
		require.Len(t, got, 1, name)
		assert.Equal(t, got[0].MCP.SessionID, "chat-2", name)
	}
	v, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.True(t, v.OK, v.Reason)
	assert.Equal(t, v.Checked, len(all)+3)
	recent, err := st.ListEvents(ctx, accounts.EventFilter{Since: now.Add(-time.Second), Limit: 1000})
	require.NoError(t, err)
	assert.True(t, len(recent) >= 1 && len(recent) < len(all), "time filter")
	n, err := st.PruneEvents(ctx, now.Add(-time.Second))
	require.NoError(t, err)
	assert.True(t, n > 0, "retention prunes old entries")
	v, err = accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.True(t, v.OK, "the chain still verifies from its first remaining entry")

	// Repository registry: one graph, referenced by two organisations.
	add := func(orgID, graph, vis string) error {
		creator := bob.ID
		if orgID == dup.ID {
			creator = ada.ID
		}
		return st.AddRepository(ctx, accounts.Repository{OrgID: orgID, Graph: graph, Remote: "https://github.com/" + graph, Visibility: vis, AddedBy: creator, AddedAt: now})
	}
	require.NoError(t, add(team.ID, "acme/api", accounts.Public))
	require.NoError(t, add(dup.ID, "acme/api", accounts.Public))
	require.NoError(t, add(team.ID, "acme/web", accounts.Private))
	assert.True(t, errors.Is(add(team.ID, "acme/api", accounts.Public), accounts.ErrExists))
	assert.True(t, errors.Is(add("no-such-org", "acme/api", accounts.Public), accounts.ErrForbidden))
	repos, err := st.OrgRepositories(ctx, team.ID)
	require.NoError(t, err)
	require.Len(t, repos, 2)
	assert.Equal(t, repos[0].Graph, "acme/api")
	assert.Equal(t, repos[1].Visibility, accounts.Private)
	refs, err := st.GraphRepositories(ctx, "acme/api")
	require.NoError(t, err)
	assert.Equal(t, len(refs), 2)
	assert.Equal(t, repos[1].Access, accounts.AccessOK, "new entries are usable")
	assert.True(t, repos[1].VerifiedAt == nil)
	require.NoError(t, st.SetRepositoryAccess(ctx, team.ID, "acme/web", accounts.AccessLost, now))
	repos, err = st.OrgRepositories(ctx, team.ID)
	require.NoError(t, err)
	assert.False(t, repos[1].Usable(), "a lost private repository is not usable")
	require.NotNil(t, repos[1].VerifiedAt)
	assert.True(t, repos[1].VerifiedAt.Equal(now))
	assert.True(t, errors.Is(st.SetRepositoryAccess(ctx, dup.ID, "acme/web", accounts.AccessOK, now), accounts.ErrNotFound))

	// Organisation Git tokens: sealed, admins only, bound to org and host.
	_, err = svc.SetGitToken(ctx, bobP, team.ID, "github.com", "ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	assert.True(t, errors.Is(err, accounts.ErrNoSecretKey), "no secret key, no tokens")
	sealKey := make([]byte, 32)
	sealKey[0] = 7
	svc.Secrets, err = accounts.NewSealer(sealKey)
	require.NoError(t, err)
	const ghToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	gt, err := svc.SetGitToken(ctx, bobP, team.ID, " GitHub.com ", ghToken)
	require.NoError(t, err)
	assert.Equal(t, gt.Host, "github.com")
	assert.Equal(t, gt.Preview, "ghp_a***56789")
	_, err = svc.SetGitToken(ctx, accounts.Principal{User: bob, Via: "key", OrgID: team.ID}, team.ID, "github.com", ghToken)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "API keys do not manage secrets")
	_, err = svc.SetGitToken(ctx, bobP, team.ID, "https://github.com/x", ghToken)
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "a host, not a URL")
	_, err = svc.SetGitToken(ctx, bobP, team.ID, "gitlab.com", "short")
	assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
	listed, err := svc.GitTokens(ctx, bobP, team.ID)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	b, _ := json.Marshal(listed)
	assert.False(t, strings.Contains(string(b), ghToken) || strings.Contains(string(b), "sealed"), "listing shows the preview only")
	stored, err := st.GitToken(ctx, team.ID, "github.com")
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(stored.Sealed), ghToken), "stored sealed, never in clear")
	opened, err := svc.OpenGitToken(ctx, team.ID, "github.com")
	require.NoError(t, err)
	assert.Equal(t, opened, ghToken)
	stored.OrgID = dup.ID
	require.NoError(t, st.PutGitToken(ctx, stored))
	_, err = svc.OpenGitToken(ctx, dup.ID, "github.com")
	assert.Error(t, err, "a sealed token copied to another organisation does not open")
	_, err = svc.OpenGitToken(ctx, team.ID, "gitlab.com")
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	require.NoError(t, svc.RemoveGitToken(ctx, bobP, team.ID, "github.com"))
	assert.True(t, errors.Is(svc.RemoveGitToken(ctx, bobP, team.ID, "github.com"), accounts.ErrNotFound))
	left, err := st.RemoveRepository(ctx, team.ID, "acme/api")
	require.NoError(t, err)
	assert.Equal(t, left, 1, "the other organisation still uses the graph")
	left, err = st.RemoveRepository(ctx, dup.ID, "acme/api")
	require.NoError(t, err)
	assert.Equal(t, left, 0, "the last reference is gone")
	_, err = st.RemoveRepository(ctx, dup.ID, "acme/api")
	assert.True(t, errors.Is(err, accounts.ErrNotFound))

	orgs, err := st.ListOrgs(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, len(orgs), 6, "including the setup organisation")
	users, err := st.ListUsers(ctx, 100)
	require.NoError(t, err)
	assert.Equal(t, users[0].Email, "ada@example.com", "by email")
}

// RunConcurrentAudit appends from many goroutines and checks the chain did
// not fork.
func RunConcurrentAudit(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	svc := accounts.NewService(st, true)
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 10 {
				_, err := svc.Record(ctx, accounts.Event{Action: "concurrent", Detail: fmt.Sprintf("%d-%d", g, i)})
				assert.NoError(t, err)
			}
		})
	}
	wg.Wait()
	v, err := accounts.VerifyAudit(ctx, st)
	require.NoError(t, err)
	assert.True(t, v.OK, v.Reason)
	assert.True(t, v.Checked >= 160)
}
