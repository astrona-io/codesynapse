// Package memstore keeps accounts in memory, for tests and for running
// without PostgreSQL. It follows the accountstest contract.
package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Store is safe for concurrent use.
type Store struct {
	mu             sync.RWMutex
	policy         bool
	outbox         map[string]int64
	users          map[string]accounts.User
	seen           map[string]time.Time // notifications seen, per user
	orgs           map[string]accounts.Org
	members        map[string]map[string]member // org -> user -> role
	sessions       map[string]accounts.Session
	executions     map[string]accounts.ExecutionSession
	keys           map[string]accounts.APIKey // by ID
	events         []accounts.Event
	lastID         int64
	repos          map[string]accounts.Repository // org|graph
	tokens         map[string]accounts.GitToken   // org|host
	teams          map[string]accounts.Team
	tuples         map[accounts.Tuple]bool // keys have CreatedBy/CreatedAt zeroed
	tupleAt        map[accounts.Tuple]accounts.Tuple
	repoPolicies   map[string]accounts.RepoPolicyApproval
	annotations    map[string]map[string]accounts.RepositoryAnnotation
	ingestRuns     map[string]ingestRecord
	ingestEntries  map[string]ingestEntry
	ingestMetrics  accounts.IngestMetricSnapshot
	ingestExportMu sync.Mutex
	setup          accounts.Setup
	idps           map[string]accounts.IdentityProvider // by org
	identities     map[string]accounts.ExternalIdentity // by ID
	ssoStates      map[string]accounts.SSOState         // by state hash
	managed        map[string]bool                      // org|user
	jobs           *jobTable
	jobsOnce       sync.Once
	sched          *scheduleTable
	schedOnce      sync.Once
}

type member struct {
	role    accounts.Role
	addedAt time.Time
}

var _ accounts.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{outbox: map[string]int64{}, users: map[string]accounts.User{}, orgs: map[string]accounts.Org{}, members: map[string]map[string]member{},
		sessions: map[string]accounts.Session{}, executions: map[string]accounts.ExecutionSession{}, keys: map[string]accounts.APIKey{}, repos: map[string]accounts.Repository{}, tokens: map[string]accounts.GitToken{},
		teams: map[string]accounts.Team{}, tuples: map[accounts.Tuple]bool{}, tupleAt: map[accounts.Tuple]accounts.Tuple{}, repoPolicies: map[string]accounts.RepoPolicyApproval{}, annotations: map[string]map[string]accounts.RepositoryAnnotation{}, ingestRuns: map[string]ingestRecord{}, ingestEntries: map[string]ingestEntry{},
		idps: map[string]accounts.IdentityProvider{}, identities: map[string]accounts.ExternalIdentity{}, ssoStates: map[string]accounts.SSOState{}, managed: map[string]bool{}}
}

// Migrate implements accounts.Store.
func (s *Store) Migrate(context.Context) error { return nil }

// CountUsers implements accounts.Store.
func (s *Store) CountUsers(context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users), nil
}

// CreateUser implements accounts.Store.
func (s *Store) CreateUser(_ context.Context, u accounts.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.users {
		if x.Email == u.Email {
			return fmt.Errorf("email: %w", accounts.ErrExists)
		}
		if u.Username != "" && x.Username == u.Username {
			return fmt.Errorf("username: %w", accounts.ErrExists)
		}
		if x.BootstrapRoot && u.BootstrapRoot {
			return fmt.Errorf("bootstrap root: %w", accounts.ErrExists)
		}
	}
	s.users[u.ID] = u
	return nil
}

// UserByID implements accounts.Store.
func (s *Store) UserByID(_ context.Context, id string) (accounts.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return accounts.User{}, fmt.Errorf("user: %w", accounts.ErrNotFound)
}

// UserByEmail implements accounts.Store.
func (s *Store) UserByEmail(_ context.Context, email string) (accounts.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.users {
		if u.Email == email {
			return u, nil
		}
	}
	return accounts.User{}, fmt.Errorf("user: %w", accounts.ErrNotFound)
}

// UserByUsername implements accounts.Store.
func (s *Store) UserByUsername(_ context.Context, username string) (accounts.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if username != "" {
		for _, u := range s.users {
			if u.Username == username {
				return u, nil
			}
		}
	}
	return accounts.User{}, fmt.Errorf("user: %w", accounts.ErrNotFound)
}

// SetUsername implements accounts.Store.
func (s *Store) SetUsername(_ context.Context, userID, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	for id, x := range s.users {
		if id != userID && username != "" && x.Username == username {
			return fmt.Errorf("username: %w", accounts.ErrExists)
		}
	}
	u.Username = username
	s.users[userID] = u
	return nil
}

// SetPassword implements accounts.Store.
func (s *Store) SetPassword(_ context.Context, userID, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	u.PasswordHash = hash
	s.users[userID] = u
	return nil
}

// ListUsers implements accounts.Store (by email).
func (s *Store) ListUsers(_ context.Context, limit int) ([]accounts.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]accounts.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b accounts.User) int { return cmp.Compare(a.Email, b.Email) })
	return out[:min(len(out), limit)], nil
}

// CreateOrg implements accounts.Store.
func (s *Store) CreateOrg(_ context.Context, o accounts.Org, owner string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.orgs {
		if x.Slug == o.Slug {
			return fmt.Errorf("slug: %w", accounts.ErrExists)
		}
	}
	o.DefaultRepoAccess = cmp.Or(o.DefaultRepoAccess, accounts.DefaultRepoNone)
	o.PolicyRevision = max(o.PolicyRevision, 1)
	s.orgs[o.ID] = o
	s.outbox[o.ID] = o.PolicyRevision
	s.members[o.ID] = map[string]member{owner: {role: accounts.Owner, addedAt: o.CreatedAt}}
	return nil
}

// OrgBySlug implements accounts.Store.
func (s *Store) OrgBySlug(_ context.Context, slug string) (accounts.Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.orgs {
		if o.Slug == slug {
			return o, nil
		}
	}
	return accounts.Org{}, fmt.Errorf("organisation: %w", accounts.ErrNotFound)
}

// OrgByID implements accounts.Store.
func (s *Store) OrgByID(_ context.Context, id string) (accounts.Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if o, ok := s.orgs[id]; ok {
		return o, nil
	}
	return accounts.Org{}, fmt.Errorf("organisation: %w", accounts.ErrNotFound)
}

// ListOrgs implements accounts.Store (by slug).
func (s *Store) ListOrgs(_ context.Context, limit int) ([]accounts.Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]accounts.Org, 0, len(s.orgs))
	for _, o := range s.orgs {
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b accounts.Org) int { return cmp.Compare(a.Slug, b.Slug) })
	return out[:min(len(out), limit)], nil
}

// UserMemberships implements accounts.Store.
func (s *Store) UserMemberships(_ context.Context, userID string) ([]accounts.Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Membership{}
	for orgID, ms := range s.members {
		if m, ok := ms[userID]; ok {
			out = append(out, accounts.Membership{Org: s.orgs[orgID], User: public(s.users[userID]), Role: m.role, AddedAt: m.addedAt})
		}
	}
	slices.SortFunc(out, func(a, b accounts.Membership) int {
		return cmp.Or(cmp.Compare(a.Org.Name, b.Org.Name), cmp.Compare(a.Org.ID, b.Org.ID))
	})
	return out, nil
}

// OrgMembers implements accounts.Store.
func (s *Store) OrgMembers(_ context.Context, orgID string) ([]accounts.Membership, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Membership{}
	for userID, m := range s.members[orgID] {
		out = append(out, accounts.Membership{Org: s.orgs[orgID], User: public(s.users[userID]), Role: m.role, AddedAt: m.addedAt})
	}
	slices.SortFunc(out, func(a, b accounts.Membership) int { return cmp.Compare(a.User.Email, b.User.Email) })
	return out, nil
}

// MemberRole implements accounts.Store.
func (s *Store) MemberRole(_ context.Context, orgID, userID string) (accounts.Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.members[orgID][userID]; ok {
		return m.role, nil
	}
	return "", fmt.Errorf("membership: %w", accounts.ErrNotFound)
}

// SetMember implements accounts.Store.
func (s *Store) SetMember(ctx context.Context, orgID, userID string, role accounts.Role) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		orgIDs := []string{orgID}
		if err := st.(*Store).setMember(ctx, orgID, userID, role); err != nil {
			return err
		}
		for _, id := range orgIDs {
			if err := st.TouchPolicy(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) setMember(_ context.Context, orgID, userID string, role accounts.Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	m, ok := s.members[orgID][userID]
	if !ok {
		m.addedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	m.role = role
	s.members[orgID][userID] = m
	return nil
}

// RemoveMember implements accounts.Store.
func (s *Store) RemoveMember(ctx context.Context, orgID, userID string) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		orgIDs := []string{orgID}
		if err := st.(*Store).removeMember(ctx, orgID, userID); err != nil {
			return err
		}
		for _, id := range orgIDs {
			if err := st.TouchPolicy(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) removeMember(_ context.Context, orgID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.members[orgID][userID]; !ok {
		return fmt.Errorf("membership: %w", accounts.ErrNotFound)
	}
	delete(s.members[orgID], userID)
	delete(s.managed, orgID+"|"+userID)
	for tuple := range s.tuples {
		org, _, ok := accounts.SplitRepoObject(tuple.ObjectID)
		if ok && tuple.ObjectType == accounts.TypeRepository && org == orgID && tuple.SubjectType == accounts.TypeUser && tuple.SubjectID == userID {
			delete(s.tuples, tuple)
			delete(s.tupleAt, tuple)
		}
	}

	return nil
}

// CreateSession implements accounts.Store.
func (s *Store) CreateSession(_ context.Context, sess accounts.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.TokenHash] = sess
	return nil
}

// SessionByHash implements accounts.Store.
func (s *Store) SessionByHash(_ context.Context, hash string) (accounts.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if sess, ok := s.sessions[hash]; ok {
		return sess, nil
	}
	return accounts.Session{}, fmt.Errorf("session: %w", accounts.ErrNotFound)
}

// DeleteSession implements accounts.Store.
func (s *Store) DeleteSession(_ context.Context, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, hash)
	return nil
}

// DeleteUserSessions implements accounts.Store.
func (s *Store) DeleteUserSessions(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if sess.UserID == userID {
			delete(s.sessions, h)
		}
	}
	return nil
}

// CreateAPIKey implements accounts.Store.
func (s *Store) CreateAPIKey(_ context.Context, k accounts.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.ID] = k
	return nil
}

// APIKeyByHash implements accounts.Store.
func (s *Store) APIKeyByHash(_ context.Context, hash string) (accounts.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.keys {
		if k.Hash == hash {
			return k, nil
		}
	}
	return accounts.APIKey{}, fmt.Errorf("key: %w", accounts.ErrNotFound)
}

// ListAPIKeys implements accounts.Store (newest first).
func (s *Store) ListAPIKeys(_ context.Context, userID string) ([]accounts.APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.APIKey{}
	for _, k := range s.keys {
		if k.UserID == userID {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b accounts.APIKey) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// RevokeAPIKey implements accounts.Store.
func (s *Store) RevokeAPIKey(_ context.Context, userID, keyID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[keyID]
	if !ok || k.UserID != userID {
		return fmt.Errorf("key: %w", accounts.ErrNotFound)
	}
	if k.RevokedAt == nil {
		k.RevokedAt = &at
		s.keys[keyID] = k
	}
	return nil
}

// TouchAPIKey implements accounts.Store.
func (s *Store) TouchAPIKey(_ context.Context, keyID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k, ok := s.keys[keyID]; ok {
		k.LastUsedAt = &at
		s.keys[keyID] = k
	}
	return nil
}

// AddEvent implements accounts.Store.
func (s *Store) AddEvent(_ context.Context, e accounts.Event) (accounts.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := ""
	if n := len(s.events); n > 0 {
		prev = s.events[n-1].Hash
	}
	e = e.Prepare(prev)
	s.lastID++
	e.ID = s.lastID
	s.events = append(s.events, e)
	return e, nil
}

// ListEvents implements accounts.Store.
func (s *Store) ListEvents(_ context.Context, f accounts.EventFilter) ([]accounts.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	match := func(e accounts.Event) bool {
		return (f.OrgID == "" || e.OrgID == f.OrgID) && (f.Since.IsZero() || !e.At.Before(f.Since)) &&
			(f.Until.IsZero() || e.At.Before(f.Until)) && e.ID > f.AfterID &&
			strings.HasPrefix(e.Action, f.Action) && (f.Actor == "" || e.ActorEmail == f.Actor || e.UserID == f.Actor) &&
			(f.Outcome == "" || e.Outcome == f.Outcome) && mcpMatch(e.MCP, f)
	}
	out := []accounts.Event{}
	if f.Ascending {
		for _, e := range s.events {
			if match(e) && len(out) < limit {
				out = append(out, e)
			}
		}
		return out, nil
	}
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		if match(s.events[i]) {
			out = append(out, s.events[i])
		}
	}
	return out, nil
}

// PruneEvents implements accounts.Store.
func (s *Store) PruneEvents(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.events[:0]
	n := 0
	for _, e := range s.events {
		if e.At.Before(before) {
			n++
			continue
		}
		keep = append(keep, e)
	}
	s.events = keep
	return n, nil
}

// AddRepository implements accounts.Store.
func (s *Store) AddRepository(ctx context.Context, r accounts.Repository) error {
	return s.PolicyTransaction(ctx, r.OrgID, func(st accounts.Store) error {
		if r.AddedBy == "" {
			return accounts.ErrInvalidInput
		}
		u, err := st.UserByID(ctx, r.AddedBy)
		if err != nil || u.DisabledAt != nil {
			return accounts.ErrForbidden
		}
		if _, err := st.MemberRole(ctx, r.OrgID, r.AddedBy); err != nil {
			return accounts.ErrForbidden
		}
		tx := st.(*Store)
		if err := tx.addRepository(ctx, r); err != nil {
			return err
		}
		if err := st.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(r.OrgID, r.Graph), Relation: accounts.RepoAdmin, SubjectType: accounts.TypeUser, SubjectID: r.AddedBy, CreatedBy: r.AddedBy, CreatedAt: r.AddedAt}); err != nil {
			return err
		}
		if _, err := st.AddEvent(ctx, accounts.Event{At: r.AddedAt, UserID: r.AddedBy, ActorEmail: u.Email, OrgID: r.OrgID, Action: "repo.admin.initial", Target: r.Graph, Outcome: accounts.Success}); err != nil {
			return err
		}
		return st.TouchPolicy(ctx, r.OrgID)
	})
}

func (s *Store) addRepository(_ context.Context, r accounts.Repository) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[r.OrgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	k := r.OrgID + "|" + r.Graph
	if _, ok := s.repos[k]; ok {
		return fmt.Errorf("repository: %w", accounts.ErrExists)
	}
	r.Access = cmp.Or(r.Access, accounts.AccessOK)
	s.repos[k] = r
	return nil
}

// SetRepositoryAccess implements accounts.Store.
func (s *Store) SetRepositoryAccess(ctx context.Context, orgID, graph, access string, at time.Time) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		orgIDs := []string{orgID}
		if err := st.(*Store).setRepositoryAccess(ctx, orgID, graph, access, at); err != nil {
			return err
		}
		for _, id := range orgIDs {
			if err := st.TouchPolicy(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) setRepositoryAccess(_ context.Context, orgID, graph, access string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := orgID + "|" + graph
	r, ok := s.repos[k]
	if !ok {
		return fmt.Errorf("repository: %w", accounts.ErrNotFound)
	}
	r.Access, r.VerifiedAt = access, &at
	s.repos[k] = r
	return nil
}

// PutGitToken implements accounts.Store.
func (s *Store) PutGitToken(_ context.Context, t accounts.GitToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[t.OrgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	t.Sealed = slices.Clone(t.Sealed)
	s.tokens[t.OrgID+"|"+t.Host] = t
	return nil
}

// GitToken implements accounts.Store.
func (s *Store) GitToken(_ context.Context, orgID, host string) (accounts.GitToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tokens[orgID+"|"+host]
	if !ok {
		return accounts.GitToken{}, fmt.Errorf("git token: %w", accounts.ErrNotFound)
	}
	t.Sealed = slices.Clone(t.Sealed)
	return t, nil
}

// GitTokens implements accounts.Store.
func (s *Store) GitTokens(_ context.Context, orgID string) ([]accounts.GitToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.GitToken{}
	for _, t := range s.tokens {
		if t.OrgID == orgID {
			t.Sealed = slices.Clone(t.Sealed)
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b accounts.GitToken) int { return cmp.Compare(a.Host, b.Host) })
	return out, nil
}

// DeleteGitToken implements accounts.Store.
func (s *Store) DeleteGitToken(_ context.Context, orgID, host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := orgID + "|" + host
	if _, ok := s.tokens[k]; !ok {
		return fmt.Errorf("git token: %w", accounts.ErrNotFound)
	}
	delete(s.tokens, k)
	return nil
}

// RemoveRepository implements accounts.Store.
func (s *Store) RemoveRepository(ctx context.Context, orgID, graph string) (int, error) {
	var count int
	err := s.PolicyTransaction(ctx, orgID, func(st accounts.Store) error {
		var err error
		count, err = st.(*Store).removeRepository(ctx, orgID, graph)
		if err != nil {
			return err
		}
		if _, err := st.DeleteTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(orgID, graph)}); err != nil {
			return err
		}
		return st.TouchPolicy(ctx, orgID)
	})
	return count, err
}

func (s *Store) removeRepository(_ context.Context, orgID, graph string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := orgID + "|" + graph
	if _, ok := s.repos[k]; !ok {
		return 0, fmt.Errorf("repository: %w", accounts.ErrNotFound)
	}
	delete(s.repos, k)
	delete(s.repoPolicies, k)
	delete(s.annotations, k)
	s.expireIngestReports(orgID, graph)
	n := 0
	for _, r := range s.repos {
		if r.Graph == graph {
			n++
		}
	}
	return n, nil
}

// OrgRepositories implements accounts.Store (by graph).
func (s *Store) OrgRepositories(_ context.Context, orgID string) ([]accounts.Repository, error) {
	return s.reposWhere(func(r accounts.Repository) bool { return r.OrgID == orgID }), nil
}

// GraphRepositories implements accounts.Store (by organisation).
func (s *Store) GraphRepositories(_ context.Context, graph string) ([]accounts.Repository, error) {
	return s.reposWhere(func(r accounts.Repository) bool { return r.Graph == graph }), nil
}

func (s *Store) reposWhere(keep func(accounts.Repository) bool) []accounts.Repository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Repository{}
	for _, r := range s.repos {
		if keep(r) {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b accounts.Repository) int {
		return cmp.Or(cmp.Compare(a.Graph, b.Graph), cmp.Compare(a.OrgID, b.OrgID))
	})
	return out
}

// public drops the password hash, which never leaves the store in a membership.
func public(u accounts.User) accounts.User {
	u.PasswordHash = ""
	return u
}

// CreateTeam implements accounts.Store.
func (s *Store) CreateTeam(ctx context.Context, t accounts.Team) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {

		if err := st.(*Store).createTeam(ctx, t); err != nil {
			return err
		}
		return st.TouchPolicy(ctx, t.OrgID)
	})
}

func (s *Store) createTeam(_ context.Context, t accounts.Team) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[t.OrgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	for _, x := range s.teams {
		if x.OrgID == t.OrgID && x.Slug == t.Slug {
			return fmt.Errorf("team: %w", accounts.ErrExists)
		}
	}
	s.teams[t.ID] = t
	return nil
}

// TeamByID implements accounts.Store.
func (s *Store) TeamByID(_ context.Context, id string) (accounts.Team, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.teams[id]
	if !ok {
		return accounts.Team{}, fmt.Errorf("team: %w", accounts.ErrNotFound)
	}
	return t, nil
}

// TeamBySlug implements accounts.Store.
func (s *Store) TeamBySlug(_ context.Context, orgID, slug string) (accounts.Team, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.teams {
		if t.OrgID == orgID && t.Slug == slug {
			return t, nil
		}
	}
	return accounts.Team{}, fmt.Errorf("team: %w", accounts.ErrNotFound)
}

// OrgTeams implements accounts.Store (by name).
func (s *Store) OrgTeams(_ context.Context, orgID string) ([]accounts.Team, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Team{}
	for _, t := range s.teams {
		if t.OrgID == orgID {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b accounts.Team) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out, nil
}

// DeleteTeam implements accounts.Store.
func (s *Store) DeleteTeam(ctx context.Context, id string) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		team, err := st.TeamByID(ctx, id)
		if err != nil {
			return err
		}
		if err := st.(*Store).deleteTeam(ctx, id); err != nil {
			return err
		}
		return st.TouchPolicy(ctx, team.OrgID)
	})
}

func (s *Store) deleteTeam(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.teams[id]; !ok {
		return fmt.Errorf("team: %w", accounts.ErrNotFound)
	}
	delete(s.teams, id)
	for k := range s.tuples {
		if (k.ObjectType == accounts.TypeTeam && k.ObjectID == id) || (k.SubjectType == accounts.TypeTeam && k.SubjectID == id) {
			delete(s.tuples, k)
			delete(s.tupleAt, k)
		}
	}
	return nil
}

func tupleKey(t accounts.Tuple) accounts.Tuple {
	t.CreatedBy, t.CreatedAt = "", time.Time{}
	return t
}

// WriteTuple implements accounts.Store.
func (s *Store) WriteTuple(ctx context.Context, t accounts.Tuple) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		orgs, err := tupleOrganisations(ctx, st, []accounts.Tuple{t})
		if err != nil {
			return err
		}
		if err := st.(*Store).writeTuple(ctx, t); err != nil {
			return err
		}
		for org := range orgs {
			if err := st.TouchPolicy(ctx, org); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) writeTuple(_ context.Context, t accounts.Tuple) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := tupleKey(t)
	if t.ObjectType == accounts.TypeRepository {
		org, graph, ok := accounts.SplitRepoObject(t.ObjectID)
		if !ok {
			return accounts.ErrNotFound
		}
		if _, ok := s.repos[accounts.RepoObject(org, graph)]; !ok {
			return accounts.ErrNotFound
		}
		if t.SubjectType == accounts.TypeUser {
			if _, ok := s.members[org][t.SubjectID]; !ok {
				return accounts.ErrNotFound
			}
		}
		if t.SubjectType == accounts.TypeTeam {
			if team, ok := s.teams[t.SubjectID]; !ok || team.OrgID != org {
				return accounts.ErrNotFound
			}
		}

		for old := range s.tuples {
			if old.ObjectType == t.ObjectType && old.ObjectID == t.ObjectID && old.SubjectType == t.SubjectType && old.SubjectID == t.SubjectID && old.SubjectRelation == t.SubjectRelation {
				return accounts.ErrExists
			}
		}
	}

	if s.tuples[k] {
		return fmt.Errorf("tuple: %w", accounts.ErrExists)
	}
	s.tuples[k] = true
	s.tupleAt[k] = t
	return nil
}

// DeleteTuple implements accounts.Store.
func (s *Store) DeleteTuple(ctx context.Context, t accounts.Tuple) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		orgs, err := tupleOrganisations(ctx, st, []accounts.Tuple{t})
		if err != nil {
			return err
		}
		if err := st.(*Store).deleteTuple(ctx, t); err != nil {
			return err
		}
		for org := range orgs {
			if err := st.TouchPolicy(ctx, org); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) deleteTuple(_ context.Context, t accounts.Tuple) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := tupleKey(t)
	if !s.tuples[k] {
		return fmt.Errorf("tuple: %w", accounts.ErrNotFound)
	}
	delete(s.tuples, k)
	delete(s.tupleAt, k)
	return nil
}

func matches(t accounts.Tuple, f accounts.TupleFilter) bool {
	eq := func(want, got string) bool { return want == "" || want == got }
	if !eq(f.ObjectType, t.ObjectType) || !eq(f.ObjectID, t.ObjectID) || !eq(f.Relation, t.Relation) ||
		!eq(f.SubjectType, t.SubjectType) || !eq(f.SubjectID, t.SubjectID) || !eq(f.SubjectRelation, t.SubjectRelation) {
		return false
	}
	return (f.SubjectIDs == nil || slices.Contains(f.SubjectIDs, t.SubjectID)) && (f.ObjectIDs == nil || slices.Contains(f.ObjectIDs, t.ObjectID))
}

// ReadTuples implements accounts.Store, ordered like the PostgreSQL store.
func (s *Store) ReadTuples(_ context.Context, f accounts.TupleFilter) ([]accounts.Tuple, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Tuple{}
	for k := range s.tuples {
		if matches(k, f) {
			out = append(out, s.tupleAt[k])
		}
	}
	slices.SortFunc(out, func(a, b accounts.Tuple) int {
		return cmp.Or(cmp.Compare(a.ObjectType, b.ObjectType), cmp.Compare(a.ObjectID, b.ObjectID), cmp.Compare(a.Relation, b.Relation),
			cmp.Compare(a.SubjectType, b.SubjectType), cmp.Compare(a.SubjectID, b.SubjectID), cmp.Compare(a.SubjectRelation, b.SubjectRelation))
	})
	if len(out) > accounts.MaxTuples {
		return nil, accounts.ErrAccessLimit
	}
	return out, nil
}

// DeleteTuples implements accounts.Store.
func (s *Store) DeleteTuples(ctx context.Context, f accounts.TupleFilter) (int, error) {
	var count int
	err := s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		ts, err := st.ReadTuples(ctx, f)
		if err != nil {
			return err
		}
		tx := st.(*Store)
		orgs, err := tupleOrganisations(ctx, st, ts)
		if err != nil {
			return err
		}
		count, err = tx.deleteTuples(ctx, f)
		if err != nil {
			return err
		}
		for org := range orgs {
			if err := st.TouchPolicy(ctx, org); err != nil {
				return err
			}
		}
		return nil
	})
	return count, err
}

func (s *Store) deleteTuples(_ context.Context, f accounts.TupleFilter) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.tuples {
		if matches(k, f) {
			delete(s.tuples, k)
			delete(s.tupleAt, k)
			n++
		}
	}
	return n, nil
}

// SetUserDisabled implements accounts.Store.
func (s *Store) SetUserDisabled(ctx context.Context, userID string, at *time.Time) error {
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		ms, err := st.UserMemberships(ctx, userID)
		if err != nil {
			return err
		}
		orgIDs := []string{}
		for _, m := range ms {
			orgIDs = append(orgIDs, m.Org.ID)
		}
		if err := st.(*Store).setUserDisabled(ctx, userID, at); err != nil {
			return err
		}
		for _, id := range orgIDs {
			if err := st.TouchPolicy(ctx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) setUserDisabled(_ context.Context, userID string, at *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	u.DisabledAt = at
	s.users[userID] = u
	return nil
}

// SetPlatformAdmin implements accounts.Store.
func (s *Store) SetPlatformAdmin(_ context.Context, userID string, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	u.PlatformAdmin = admin
	s.users[userID] = u
	return nil
}

func mcpMatch(m *accounts.MCPRequest, f accounts.EventFilter) bool {
	if f.Session == "" && f.Tool == "" && f.Repo == "" {
		return true
	}
	return m != nil && (f.Session == "" || m.SessionID == f.Session) && (f.Tool == "" || m.Name == f.Tool) && (f.Repo == "" || m.Repo == f.Repo)
}
