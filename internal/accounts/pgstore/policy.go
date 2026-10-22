package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

type database interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Begin(context.Context) (pgx.Tx, error)
}

// PolicyTransaction commits a policy change, audit chain and projection outbox
// together. The lock also serializes last-administrator checks across replicas.
func (s *Store) PolicyTransaction(ctx context.Context, _ string, fn func(accounts.Store) error) error {
	if s.policy {
		return fn(s)
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1668244582)"); err != nil {
			return err
		}
		return fn(&Store{db: tx, pool: s.pool, policy: true})
	})
}

// TouchPolicy invalidates the rebuildable access projection.
func (s *Store) TouchPolicy(ctx context.Context, orgID string) error {
	return s.PolicyTransaction(ctx, orgID, func(st accounts.Store) error {
		tx := st.(*Store)
		var revision int64
		err := tx.db.QueryRow(ctx, "UPDATE orgs SET policy_revision = policy_revision + 1 WHERE id = $1 RETURNING policy_revision", orgID).Scan(&revision)
		if err != nil {
			return mapErr(err, "organisation")
		}
		_, err = tx.db.Exec(ctx, "INSERT INTO access_policy_outbox (org_id, revision) VALUES ($1, $2) ON CONFLICT (org_id) DO UPDATE SET revision = EXCLUDED.revision", orgID, revision)
		return err
	})
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

func (s *Store) setDefaultRepoAccess(ctx context.Context, orgID, policy string) error {
	if policy != accounts.DefaultRepoNone && policy != accounts.DefaultRepoReadOnly {
		return accounts.ErrInvalidInput
	}
	tag, err := s.db.Exec(ctx, "UPDATE orgs SET default_repo_access = $2 WHERE id = $1", orgID, policy)
	return affected(tag, err, "organisation")
}

// PendingAccessPolicies lists the latest revision of each dirty organisation.
func (s *Store) PendingAccessPolicies(ctx context.Context, limit int) ([]accounts.Org, error) {
	rows, err := s.db.Query(ctx, "SELECT o.id,o.slug,o.name,o.created_at,o.default_repo_access,o.policy_revision FROM orgs o JOIN access_policy_outbox p ON p.org_id=o.id ORDER BY o.id LIMIT $1", min(max(limit, 1), accounts.MaxTuples))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Org, error) { return scanOrg(r) })
}

// AcknowledgeAccessPolicy preserves newer changes while a projection is rebuilt.
func (s *Store) AcknowledgeAccessPolicy(ctx context.Context, orgID string, revision int64) error {
	_, err := s.db.Exec(ctx, "DELETE FROM access_policy_outbox WHERE org_id=$1 AND revision=$2", orgID, revision)
	return err
}

// TeamsByIDs fetches the user's bounded team set in one indexed lookup.
func (s *Store) TeamsByIDs(ctx context.Context, ids []string) ([]accounts.Team, error) {
	if len(ids) > accounts.MaxTeams {
		return nil, accounts.ErrAccessLimit
	}
	rows, err := s.db.Query(ctx, "SELECT id,org_id,slug,name,created_at FROM teams WHERE id=ANY($1) ORDER BY id", ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Team, error) {
		var t accounts.Team
		err := r.Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.CreatedAt)
		t.CreatedAt = t.CreatedAt.UTC()
		return t, err
	})
}

// RepositoriesForOrgs fetches the caller's tenant entries without per-org queries.
func (s *Store) RepositoriesForOrgs(ctx context.Context, ids []string) ([]accounts.Repository, error) {
	if len(ids) > accounts.MaxMemberships {
		return nil, accounts.ErrAccessLimit
	}
	rows, err := s.db.Query(ctx, "SELECT "+repoCols+" FROM repositories WHERE org_id=ANY($1) ORDER BY org_id,graph LIMIT $2", ids, accounts.MaxTuples+1)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Repository, error) { return scanRepo(r) })
	if len(out) > accounts.MaxTuples {
		return nil, accounts.ErrAccessLimit
	}
	return out, err
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

// ReadSnapshot uses MVCC so authorization reads never take the mutation lock.
func (s *Store) ReadSnapshot(ctx context.Context, fn func(accounts.Store) error) error {
	if s.policy {
		return fn(s)
	}
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		return fn(&Store{db: tx, pool: s.pool, policy: true})
	})
}
