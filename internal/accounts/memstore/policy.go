package memstore

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PolicyTransaction evaluates changes against a private copy and publishes only
// on success, so test stores have the same rollback behavior as PostgreSQL.
func (s *Store) PolicyTransaction(ctx context.Context, _ string, fn func(accounts.Store) error) error {
	if s.policy {
		return fn(s)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := s.snapshot()
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.users = tx.users
	s.orgs = tx.orgs
	s.members = tx.members
	s.sessions = tx.sessions
	s.executions = tx.executions
	s.keys = tx.keys
	s.events = tx.events
	s.lastID = tx.lastID
	s.tip = tx.tip
	s.segments = tx.segments
	s.segmentID = tx.segmentID
	s.repos = tx.repos
	s.tokens = tx.tokens
	s.userTokens = tx.userTokens
	s.teams = tx.teams
	s.tuples = tx.tuples
	s.tupleAt = tx.tupleAt
	s.outbox = tx.outbox
	s.repoPolicies = tx.repoPolicies
	s.memberRepos = tx.memberRepos
	s.annotations = tx.annotations
	s.triage = tx.triage
	s.ingestRuns = tx.ingestRuns
	s.ingestEntries = tx.ingestEntries
	s.ingestMetrics = tx.ingestMetrics
	s.setup = tx.setup
	s.gateway, s.localFirst, s.localExclude = tx.gateway, tx.localFirst, tx.localExclude
	s.idps, s.identities, s.ssoStates, s.managed = tx.idps, tx.identities, tx.ssoStates, tx.managed
	return nil
}

// TouchPolicy invalidates the rebuildable access projection.
func (s *Store) TouchPolicy(_ context.Context, orgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[orgID]
	if !ok {
		return accounts.ErrNotFound
	}
	o.PolicyRevision++
	s.orgs[orgID] = o
	s.outbox[orgID] = o.PolicyRevision
	return nil
}

// SetDefaultRepoAccess stores the organisation fallback.
func (s *Store) SetDefaultRepoAccess(ctx context.Context, orgID, policy string) error {
	return s.PolicyTransaction(ctx, orgID, func(st accounts.Store) error {
		if err := st.(*Store).setDefaultRepoAccess(ctx, orgID, policy); err != nil {
			return err
		}
		return st.TouchPolicy(ctx, orgID)
	})
}

func (s *Store) setDefaultRepoAccess(_ context.Context, orgID, policy string) error {
	if policy != accounts.DefaultRepoNone && policy != accounts.DefaultRepoReadOnly {
		return accounts.ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orgs[orgID]
	if !ok {
		return accounts.ErrNotFound
	}
	o.DefaultRepoAccess = policy
	s.orgs[orgID] = o
	return nil
}

// PendingAccessPolicies lists the latest revision of each dirty organisation.
func (s *Store) PendingAccessPolicies(_ context.Context, limit int) ([]accounts.Org, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Org{}
	for id := range s.outbox {
		out = append(out, s.orgs[id])
	}
	slices.SortFunc(out, func(a, b accounts.Org) int { return cmp.Compare(a.ID, b.ID) })
	return out[:min(len(out), min(max(limit, 1), accounts.MaxTuples))], nil
}

// AcknowledgeAccessPolicy preserves changes newer than the rebuilt projection.
func (s *Store) AcknowledgeAccessPolicy(_ context.Context, orgID string, revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outbox[orgID] == revision {
		delete(s.outbox, orgID)
	}
	return nil
}

// TeamsByIDs fetches the user's bounded team set in one lookup.
func (s *Store) TeamsByIDs(_ context.Context, ids []string) ([]accounts.Team, error) {
	if len(ids) > accounts.MaxTeams {
		return nil, accounts.ErrAccessLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Team{}
	for _, id := range ids {
		if t, ok := s.teams[id]; ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// RepositoriesForOrgs fetches all entries in the caller's bounded tenant set.
func (s *Store) RepositoriesForOrgs(_ context.Context, ids []string) ([]accounts.Repository, error) {
	if len(ids) > accounts.MaxMemberships {
		return nil, accounts.ErrAccessLimit
	}
	out := s.reposWhere(func(r accounts.Repository) bool { return slices.Contains(ids, r.OrgID) })
	if len(out) > accounts.MaxTuples {
		return nil, accounts.ErrAccessLimit
	}
	return out, nil
}

func tupleOrganisations(ctx context.Context, st accounts.Store, ts []accounts.Tuple) (map[string]bool, error) {
	orgs := map[string]bool{}
	for _, t := range ts {
		switch t.ObjectType {
		case accounts.TypeOrganisation:
			orgs[t.ObjectID] = true
		case accounts.TypeRepository:
			org, _, ok := accounts.SplitRepoObject(t.ObjectID)
			if !ok {
				return nil, accounts.ErrInvalidInput
			}
			orgs[org] = true
		case accounts.TypeTeam:
			team, err := st.TeamByID(ctx, t.ObjectID)
			if err != nil {
				return nil, err
			}
			orgs[team.OrgID] = true
		default:
			return nil, accounts.ErrInvalidInput
		}
	}
	return orgs, nil
}

func (s *Store) snapshot() *Store {
	tx := New()
	tx.policy = true
	tx.users = maps.Clone(s.users)
	tx.orgs = maps.Clone(s.orgs)
	tx.members = maps.Clone(s.members)
	for id, ms := range tx.members {
		tx.members[id] = maps.Clone(ms)
	}
	tx.sessions = maps.Clone(s.sessions)
	tx.executions = maps.Clone(s.executions)
	tx.keys = maps.Clone(s.keys)
	tx.events = slices.Clone(s.events)
	tx.lastID = s.lastID
	tx.tip = s.tip
	tx.segments = slices.Clone(s.segments)
	tx.segmentID = s.segmentID
	tx.repos = maps.Clone(s.repos)
	tx.tokens = maps.Clone(s.tokens)
	tx.userTokens = maps.Clone(s.userTokens)
	tx.teams = maps.Clone(s.teams)
	tx.tuples = maps.Clone(s.tuples)
	tx.tupleAt = maps.Clone(s.tupleAt)
	tx.outbox = maps.Clone(s.outbox)
	tx.repoPolicies = maps.Clone(s.repoPolicies)
	tx.memberRepos = maps.Clone(s.memberRepos)
	tx.annotations = maps.Clone(s.annotations)
	tx.triage = maps.Clone(s.triage)
	tx.ingestRuns = maps.Clone(s.ingestRuns)
	tx.ingestEntries = maps.Clone(s.ingestEntries)
	tx.ingestMetrics = s.ingestMetrics
	tx.setup = s.setup
	tx.gateway, tx.localFirst, tx.localExclude = maps.Clone(s.gateway), maps.Clone(s.localFirst), maps.Clone(s.localExclude)
	tx.idps, tx.identities, tx.ssoStates, tx.managed = maps.Clone(s.idps), maps.Clone(s.identities), maps.Clone(s.ssoStates), maps.Clone(s.managed)
	tx.jobs = s.jobTable() // shared, not transactional
	tx.sched = s.scheduleTable()
	tx.daily = s.dailyTable()
	for key, annotations := range tx.annotations {
		tx.annotations[key] = maps.Clone(annotations)
	}
	for key, rows := range tx.triage {
		tx.triage[key] = maps.Clone(rows)
	}
	return tx
}

// ReadSnapshot copies under a read lock; unrelated readers can proceed together.
func (s *Store) ReadSnapshot(ctx context.Context, fn func(accounts.Store) error) error {
	if s.policy {
		return fn(s)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	tx := s.snapshot()
	s.mu.RUnlock()
	return fn(tx)
}
