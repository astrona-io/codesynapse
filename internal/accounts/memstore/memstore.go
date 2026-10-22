// Package memstore keeps accounts in memory, for tests and for running
// without PostgreSQL. It follows the accountstest contract.
package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

// Store is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	users    map[string]accounts.User
	orgs     map[string]accounts.Org
	members  map[string]map[string]member // org -> user -> role
	sessions map[string]accounts.Session
	keys     map[string]accounts.APIKey // by ID
	events   []accounts.Event
	lastID   int64
}

type member struct {
	role    accounts.Role
	addedAt time.Time
}

var _ accounts.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{users: map[string]accounts.User{}, orgs: map[string]accounts.Org{}, members: map[string]map[string]member{},
		sessions: map[string]accounts.Session{}, keys: map[string]accounts.APIKey{}}
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
	s.orgs[o.ID] = o
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
func (s *Store) SetMember(_ context.Context, orgID, userID string, role accounts.Role) error {
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
func (s *Store) RemoveMember(_ context.Context, orgID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.members[orgID][userID]; !ok {
		return fmt.Errorf("membership: %w", accounts.ErrNotFound)
	}
	delete(s.members[orgID], userID)
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
			(f.Until.IsZero() || e.At.Before(f.Until)) && e.ID > f.AfterID
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

// public drops the password hash, which never leaves the store in a membership.
func public(u accounts.User) accounts.User {
	u.PasswordHash = ""
	return u
}
