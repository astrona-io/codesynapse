// Package pgstore keeps accounts in PostgreSQL. Migrations are embedded SQL
// files applied in order under an advisory lock, so several API replicas
// can start at once.
package pgstore

import (
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrateLock is the advisory lock key held while migrating.
const migrateLock = 0x636f6465 // "code"

// Store is a PostgreSQL accounts store.
type Store struct {
	db     database
	pool   *pgxpool.Pool
	policy bool
}

var _ accounts.Store = (*Store)(nil)

// Open connects to url (postgres://...); the password comes from the URL or
// the PG* environment, never from flags.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("database url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.ConnConfig.Tracer = tracer{}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to PostgreSQL: %w", err)
	}
	return &Store{db: pool, pool: pool}, nil
}

// OpenWait retries Open for about a minute, since PostgreSQL may still be
// starting next to us; onRetry hears about each failed attempt.
func OpenWait(ctx context.Context, url string, onRetry func(attempt int, err error)) (*Store, error) {
	var err error
	for attempt := range 30 {
		var st *Store
		if st, err = Open(ctx, url); err == nil {
			return st, nil
		}
		onRetry(attempt+1, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, err
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Migrate implements accounts.Store.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) //nolint:errcheck // released with the connection anyway
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		base := strings.TrimPrefix(name, "migrations/")
		version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration %s: name must start with a number", base)
		}
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx) //nolint:errcheck // the exec error is the one to report
			return fmt.Errorf("migration %s: %w", base, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			_ = tx.Rollback(ctx) //nolint:errcheck // as above
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// mapErr turns PostgreSQL errors into accounts errors.
func mapErr(err error, what string) error {
	var pg *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%s: %w", what, accounts.ErrNotFound)
	case errors.As(err, &pg) && pg.Code == "23505":
		return fmt.Errorf("%s: %w", what, accounts.ErrExists)
	case errors.As(err, &pg) && pg.Code == "23503":
		return fmt.Errorf("%s: %w", what, accounts.ErrNotFound)
	}
	return err
}

func affected(tag pgconn.CommandTag, err error, what string) error {
	if err != nil {
		return mapErr(err, what)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%s: %w", what, accounts.ErrNotFound)
	}
	return nil
}

// CountUsers implements accounts.Store.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n, err
}

const userCols = "id, email, name, password_hash, platform_admin, created_at, disabled_at, bootstrap_root"

func scanUser(row pgx.Row) (accounts.User, error) {
	var u accounts.User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.PlatformAdmin, &u.CreatedAt, &u.DisabledAt, &u.BootstrapRoot)
	u.CreatedAt = u.CreatedAt.UTC()
	if u.DisabledAt != nil {
		t := u.DisabledAt.UTC()
		u.DisabledAt = &t
	}
	return u, err
}

// CreateUser implements accounts.Store.
func (s *Store) CreateUser(ctx context.Context, u accounts.User) error {
	_, err := s.db.Exec(ctx, "INSERT INTO users ("+userCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		u.ID, u.Email, u.Name, u.PasswordHash, u.PlatformAdmin, u.CreatedAt, u.DisabledAt, u.BootstrapRoot)
	if isConstraint(err, "users_one_bootstrap_root") {
		return fmt.Errorf("bootstrap root: %w", accounts.ErrExists)
	}
	return mapErr(err, "email")
}

// isConstraint reports a violation of the named constraint or index.
func isConstraint(err error, name string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.ConstraintName == name
}

// MarkBootstrapRoot implements accounts.Store.
func (s *Store) MarkBootstrapRoot(ctx context.Context, userID string) error {
	tag, err := s.db.Exec(ctx, "UPDATE users SET bootstrap_root = true WHERE id = $1", userID)
	if isConstraint(err, "users_one_bootstrap_root") {
		return fmt.Errorf("bootstrap root: %w", accounts.ErrExists)
	}
	return affected(tag, err, "user")
}

// Setup implements accounts.Store.
func (s *Store) Setup(ctx context.Context) (accounts.Setup, error) {
	var st accounts.Setup
	var root, org, owner *string
	err := s.db.QueryRow(ctx, "SELECT state, root_user_id, org_id, owner_id, completed_at FROM installation_setup").
		Scan(&st.State, &root, &org, &owner, &st.CompletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.Setup{}, nil
	}
	if err != nil {
		return accounts.Setup{}, err
	}
	st.RootUserID, st.OrgID, st.OwnerID = deref(root), deref(org), deref(owner)
	if st.CompletedAt != nil {
		t := st.CompletedAt.UTC()
		st.CompletedAt = &t
	}
	return st, nil
}

// SaveSetup implements accounts.Store. A completed record is fixed by a
// trigger.
func (s *Store) SaveSetup(ctx context.Context, st accounts.Setup) error {
	_, err := s.db.Exec(ctx, `INSERT INTO installation_setup (id, state, root_user_id, org_id, owner_id, completed_at)
VALUES (true, $1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET state = $1, root_user_id = $2, org_id = $3, owner_id = $4, completed_at = $5`,
		st.State, nullable(st.RootUserID), nullable(st.OrgID), nullable(st.OwnerID), st.CompletedAt)
	return mapErr(err, "setup")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// UserByID implements accounts.Store.
func (s *Store) UserByID(ctx context.Context, id string) (accounts.User, error) {
	u, err := scanUser(s.db.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE id = $1", id))
	return u, mapErr(err, "user")
}

// UserByEmail implements accounts.Store.
func (s *Store) UserByEmail(ctx context.Context, email string) (accounts.User, error) {
	u, err := scanUser(s.db.QueryRow(ctx, "SELECT "+userCols+" FROM users WHERE email = $1", email))
	return u, mapErr(err, "user")
}

// SetPassword implements accounts.Store.
func (s *Store) SetPassword(ctx context.Context, userID, hash string) error {
	tag, err := s.db.Exec(ctx, "UPDATE users SET password_hash = $2 WHERE id = $1", userID, hash)
	return affected(tag, err, "user")
}

// ListUsers implements accounts.Store.
func (s *Store) ListUsers(ctx context.Context, limit int) ([]accounts.User, error) {
	rows, err := s.db.Query(ctx, "SELECT "+userCols+" FROM users ORDER BY email LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.User, error) { return scanUser(r) })
}

const orgCols = "id, slug, name, created_at, default_repo_access, policy_revision"

func scanOrg(row pgx.Row) (accounts.Org, error) {
	var o accounts.Org
	err := row.Scan(&o.ID, &o.Slug, &o.Name, &o.CreatedAt, &o.DefaultRepoAccess, &o.PolicyRevision)
	o.CreatedAt = o.CreatedAt.UTC()
	return o, err
}

// CreateOrg implements accounts.Store; the owner membership is created in
// the same transaction.
func (s *Store) CreateOrg(ctx context.Context, o accounts.Org, owner string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO orgs ("+orgCols+") VALUES ($1, $2, $3, $4, $5, $6)", o.ID, o.Slug, o.Name, o.CreatedAt, cmp.Or(o.DefaultRepoAccess, accounts.DefaultRepoNone), max(o.PolicyRevision, 1)); err != nil {
			return mapErr(err, "slug")
		}
		_, err := tx.Exec(ctx, "INSERT INTO memberships (org_id, user_id, role, added_at) VALUES ($1, $2, 'owner', $3)", o.ID, owner, o.CreatedAt)
		if err != nil {
			return mapErr(err, "owner")
		}
		_, err = tx.Exec(ctx, "INSERT INTO access_policy_outbox(org_id,revision) SELECT id,policy_revision FROM orgs WHERE id=$1", o.ID)
		return err
	})
}

// OrgBySlug implements accounts.Store.
func (s *Store) OrgBySlug(ctx context.Context, slug string) (accounts.Org, error) {
	o, err := scanOrg(s.db.QueryRow(ctx, "SELECT "+orgCols+" FROM orgs WHERE slug = $1", slug))
	return o, mapErr(err, "organisation")
}

// OrgByID implements accounts.Store.
func (s *Store) OrgByID(ctx context.Context, id string) (accounts.Org, error) {
	o, err := scanOrg(s.db.QueryRow(ctx, "SELECT "+orgCols+" FROM orgs WHERE id = $1", id))
	return o, mapErr(err, "organisation")
}

// ListOrgs implements accounts.Store.
func (s *Store) ListOrgs(ctx context.Context, limit int) ([]accounts.Org, error) {
	rows, err := s.db.Query(ctx, "SELECT "+orgCols+" FROM orgs ORDER BY slug LIMIT $1", limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Org, error) { return scanOrg(r) })
}

const membershipQuery = `SELECT o.id, o.slug, o.name, o.created_at, o.default_repo_access, o.policy_revision,
  u.id, u.email, u.name, u.password_hash, u.platform_admin, u.created_at, u.disabled_at, u.bootstrap_root, m.role, m.added_at
FROM memberships m JOIN orgs o ON o.id = m.org_id JOIN users u ON u.id = m.user_id `

func scanMembership(r pgx.CollectableRow) (accounts.Membership, error) {
	var m accounts.Membership
	var role string
	err := r.Scan(&m.Org.ID, &m.Org.Slug, &m.Org.Name, &m.Org.CreatedAt, &m.Org.DefaultRepoAccess, &m.Org.PolicyRevision,
		&m.User.ID, &m.User.Email, &m.User.Name, &m.User.PasswordHash, &m.User.PlatformAdmin, &m.User.CreatedAt, &m.User.DisabledAt, &m.User.BootstrapRoot, &role, &m.AddedAt)
	m.Role = accounts.Role(role)
	m.Org.CreatedAt, m.User.CreatedAt, m.AddedAt = m.Org.CreatedAt.UTC(), m.User.CreatedAt.UTC(), m.AddedAt.UTC()
	m.User.PasswordHash = "" // never leaves the store in a membership
	return m, err
}

// UserMemberships implements accounts.Store.
func (s *Store) UserMemberships(ctx context.Context, userID string) ([]accounts.Membership, error) {
	rows, err := s.db.Query(ctx, membershipQuery+"WHERE m.user_id = $1 ORDER BY o.name, o.id LIMIT 257", userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMembership)
}

// OrgMembers implements accounts.Store.
func (s *Store) OrgMembers(ctx context.Context, orgID string) ([]accounts.Membership, error) {
	rows, err := s.db.Query(ctx, membershipQuery+"WHERE m.org_id = $1 ORDER BY u.email LIMIT 5001", orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMembership)
}

// MemberRole implements accounts.Store.
func (s *Store) MemberRole(ctx context.Context, orgID, userID string) (accounts.Role, error) {
	var role string
	err := s.db.QueryRow(ctx, "SELECT role FROM memberships WHERE org_id = $1 AND user_id = $2", orgID, userID).Scan(&role)
	return accounts.Role(role), mapErr(err, "membership")
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

func (s *Store) setMember(ctx context.Context, orgID, userID string, role accounts.Role) error {
	_, err := s.db.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role, added_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`, orgID, userID, string(role), time.Now().UTC())
	return mapErr(err, "organisation or user")
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

func (s *Store) removeMember(ctx context.Context, orgID, userID string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM memberships WHERE org_id = $1 AND user_id = $2", orgID, userID)
	return affected(tag, err, "membership")
}

// CreateSession implements accounts.Store; expired sessions of the user are
// cleared on the way.
func (s *Store) CreateSession(ctx context.Context, sess accounts.Session) error {
	if _, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1 AND expires_at <= now()", sess.UserID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, "INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)",
		sess.TokenHash, sess.UserID, sess.CreatedAt, sess.ExpiresAt)
	return mapErr(err, "session")
}

// SessionByHash implements accounts.Store.
func (s *Store) SessionByHash(ctx context.Context, hash string) (accounts.Session, error) {
	var sess accounts.Session
	err := s.db.QueryRow(ctx, "SELECT token_hash, user_id, created_at, expires_at FROM sessions WHERE token_hash = $1", hash).
		Scan(&sess.TokenHash, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt)
	sess.CreatedAt, sess.ExpiresAt = sess.CreatedAt.UTC(), sess.ExpiresAt.UTC()
	return sess, mapErr(err, "session")
}

// DeleteSession implements accounts.Store.
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE token_hash = $1", hash)
	return err
}

// DeleteUserSessions implements accounts.Store.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1", userID)
	return err
}

const keyCols = "id, user_id, org_id, name, prefix, hash, created_at, last_used_at, expires_at, revoked_at"

func scanKey(row pgx.Row) (accounts.APIKey, error) {
	var k accounts.APIKey
	err := row.Scan(&k.ID, &k.UserID, &k.OrgID, &k.Name, &k.Prefix, &k.Hash, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt)
	k.CreatedAt = k.CreatedAt.UTC()
	for _, t := range []**time.Time{&k.LastUsedAt, &k.ExpiresAt, &k.RevokedAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return k, err
}

// CreateAPIKey implements accounts.Store.
func (s *Store) CreateAPIKey(ctx context.Context, k accounts.APIKey) error {
	_, err := s.db.Exec(ctx, "INSERT INTO api_keys ("+keyCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)",
		k.ID, k.UserID, k.OrgID, k.Name, k.Prefix, k.Hash, k.CreatedAt, k.LastUsedAt, k.ExpiresAt, k.RevokedAt)
	return mapErr(err, "key")
}

// APIKeyByHash implements accounts.Store.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (accounts.APIKey, error) {
	k, err := scanKey(s.db.QueryRow(ctx, "SELECT "+keyCols+" FROM api_keys WHERE hash = $1", hash))
	return k, mapErr(err, "key")
}

// ListAPIKeys implements accounts.Store.
func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]accounts.APIKey, error) {
	rows, err := s.db.Query(ctx, "SELECT "+keyCols+" FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC, id", userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.APIKey, error) { return scanKey(r) })
}

// RevokeAPIKey implements accounts.Store.
func (s *Store) RevokeAPIKey(ctx context.Context, userID, keyID string, at time.Time) error {
	tag, err := s.db.Exec(ctx, "UPDATE api_keys SET revoked_at = coalesce(revoked_at, $3) WHERE id = $1 AND user_id = $2", keyID, userID, at)
	return affected(tag, err, "key")
}

// TouchAPIKey implements accounts.Store.
func (s *Store) TouchAPIKey(ctx context.Context, keyID string, at time.Time) error {
	_, err := s.db.Exec(ctx, "UPDATE api_keys SET last_used_at = $2 WHERE id = $1", keyID, at)
	return err
}

const repoCols = "org_id, graph, remote, visibility, added_by, added_at, access, verified_at"

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

func (s *Store) addRepository(ctx context.Context, r accounts.Repository) error {
	_, err := s.db.Exec(ctx, "INSERT INTO repositories ("+repoCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		r.OrgID, r.Graph, r.Remote, r.Visibility, r.AddedBy, r.AddedAt, cmp.Or(r.Access, accounts.AccessOK), r.VerifiedAt)
	return mapErr(err, "repository")
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

func (s *Store) setRepositoryAccess(ctx context.Context, orgID, graph, access string, at time.Time) error {
	tag, err := s.db.Exec(ctx, "UPDATE repositories SET access = $3, verified_at = $4 WHERE org_id = $1 AND graph = $2", orgID, graph, access, at)
	return affected(tag, err, "repository")
}

const tokenCols = "org_id, host, preview, key_id, sealed, created_by, updated_at" //nolint:gosec // column names, not a credential

// PutGitToken implements accounts.Store.
func (s *Store) PutGitToken(ctx context.Context, t accounts.GitToken) error {
	_, err := s.db.Exec(ctx, "INSERT INTO org_git_tokens ("+tokenCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (org_id, host) DO UPDATE SET preview = $3, key_id = $4, sealed = $5, created_by = $6, updated_at = $7`,
		t.OrgID, t.Host, t.Preview, t.KeyID, t.Sealed, t.CreatedBy, t.UpdatedAt)
	return mapErr(err, "git token")
}

func scanToken(r pgx.CollectableRow) (accounts.GitToken, error) {
	var t accounts.GitToken
	err := r.Scan(&t.OrgID, &t.Host, &t.Preview, &t.KeyID, &t.Sealed, &t.CreatedBy, &t.UpdatedAt)
	t.UpdatedAt = t.UpdatedAt.UTC()
	return t, err
}

// GitToken implements accounts.Store.
func (s *Store) GitToken(ctx context.Context, orgID, host string) (accounts.GitToken, error) {
	rows, err := s.db.Query(ctx, "SELECT "+tokenCols+" FROM org_git_tokens WHERE org_id = $1 AND host = $2", orgID, host)
	if err != nil {
		return accounts.GitToken{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.GitToken{}, fmt.Errorf("git token: %w", accounts.ErrNotFound)
	}
	return t, err
}

// GitTokens implements accounts.Store.
func (s *Store) GitTokens(ctx context.Context, orgID string) ([]accounts.GitToken, error) {
	rows, err := s.db.Query(ctx, "SELECT "+tokenCols+" FROM org_git_tokens WHERE org_id = $1 ORDER BY host", orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanToken)
}

// DeleteGitToken implements accounts.Store.
func (s *Store) DeleteGitToken(ctx context.Context, orgID, host string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM org_git_tokens WHERE org_id = $1 AND host = $2", orgID, host)
	return affected(tag, err, "git token")
}

// RemoveRepository implements accounts.Store.
func (s *Store) RemoveRepository(ctx context.Context, orgID, graph string) (int, error) {
	var count int
	err := s.PolicyTransaction(ctx, orgID, func(st accounts.Store) error {
		var err error
		count, err = st.(*Store).removeRepository(ctx, orgID, graph)
		if err == nil {
			err = st.(*Store).expireIngestReports(ctx, orgID, graph)
		}
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

func (s *Store) removeRepository(ctx context.Context, orgID, graph string) (int, error) {
	tag, err := s.db.Exec(ctx, "DELETE FROM repositories WHERE org_id = $1 AND graph = $2", orgID, graph)
	if err := affected(tag, err, "repository"); err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRow(ctx, "SELECT count(*) FROM repositories WHERE graph = $1", graph).Scan(&n)
	return n, err
}

func (s *Store) repos(ctx context.Context, where string, arg string) ([]accounts.Repository, error) {
	rows, err := s.db.Query(ctx, "SELECT "+repoCols+" FROM repositories WHERE "+where+" = $1 ORDER BY graph, org_id", arg)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanRepo)
}

// OrgRepositories implements accounts.Store.
func (s *Store) OrgRepositories(ctx context.Context, orgID string) ([]accounts.Repository, error) {
	return s.repos(ctx, "org_id", orgID)
}

// GraphRepositories implements accounts.Store.
func (s *Store) GraphRepositories(ctx context.Context, graph string) ([]accounts.Repository, error) {
	return s.repos(ctx, "graph", graph)
}

const eventCols = "id, at, request_id, user_id, actor_email, via, key_id, org_id, ip, user_agent, action, target, outcome, status, detail, mcp, prev_hash, hash"

// AddEvent implements accounts.Store: the chain tip is locked, the entry is
// hashed onto it and inserted, and the tip moves, in one transaction.
func (s *Store) AddEvent(ctx context.Context, e accounts.Event) (accounts.Event, error) {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var prev string
		if err := tx.QueryRow(ctx, "SELECT hash FROM audit_head WHERE id = 1 FOR UPDATE").Scan(&prev); err != nil {
			return err
		}
		e = e.Prepare(prev)
		var mcp []byte
		if e.MCP != nil {
			var err error
			if mcp, err = json.Marshal(e.MCP); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO events (at, request_id, user_id, actor_email, via, key_id, org_id, ip, user_agent,
  action, target, outcome, status, detail, mcp, prev_hash, hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) RETURNING id`,
			e.At, e.RequestID, e.UserID, e.ActorEmail, e.Via, e.KeyID, e.OrgID, e.IP, e.UserAgent,
			e.Action, e.Target, e.Outcome, e.Status, e.Detail, mcp, e.PrevHash, e.Hash).Scan(&e.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE audit_head SET hash = $1 WHERE id = 1", e.Hash)
		return err
	})
	return e, err
}

// ListEvents implements accounts.Store.
func (s *Store) ListEvents(ctx context.Context, f accounts.EventFilter) ([]accounts.Event, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	order := "DESC"
	if f.Ascending {
		order = "ASC"
	}
	var since, until *time.Time
	if !f.Since.IsZero() {
		since = &f.Since
	}
	if !f.Until.IsZero() {
		until = &f.Until
	}
	rows, err := s.db.Query(ctx, "SELECT "+eventCols+` FROM events
WHERE ($1 = '' OR org_id = $1) AND ($2::timestamptz IS NULL OR at >= $2) AND ($3::timestamptz IS NULL OR at < $3) AND id > $4
  AND ($6 = '' OR starts_with(action, $6)) AND ($7 = '' OR actor_email = $7 OR user_id = $7) AND ($8 = '' OR outcome = $8)
  AND ($9 = '' OR mcp->>'session_id' = $9) AND ($10 = '' OR mcp->>'name' = $10) AND ($11 = '' OR mcp->>'repo' = $11)
ORDER BY id `+order+" LIMIT $5", f.OrgID, since, until, f.AfterID, limit, f.Action, f.Actor, f.Outcome, f.Session, f.Tool, f.Repo)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Event, error) {
		var e accounts.Event
		var mcp []byte
		err := r.Scan(&e.ID, &e.At, &e.RequestID, &e.UserID, &e.ActorEmail, &e.Via, &e.KeyID, &e.OrgID, &e.IP, &e.UserAgent,
			&e.Action, &e.Target, &e.Outcome, &e.Status, &e.Detail, &mcp, &e.PrevHash, &e.Hash)
		e.At = e.At.UTC()
		if err == nil && mcp != nil {
			e.MCP = &accounts.MCPRequest{}
			err = json.Unmarshal(mcp, e.MCP)
		}
		return e, err
	})
}

// PruneEvents implements accounts.Store.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.db.Exec(ctx, "DELETE FROM events WHERE at < $1", before)
	return int(tag.RowsAffected()), err
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

func (s *Store) createTeam(ctx context.Context, t accounts.Team) error {
	_, err := s.db.Exec(ctx, "INSERT INTO teams (id, org_id, slug, name, created_at) VALUES ($1, $2, $3, $4, $5)", t.ID, t.OrgID, t.Slug, t.Name, t.CreatedAt)
	return mapErr(err, "team")
}

func (s *Store) team(ctx context.Context, where string, args ...any) (accounts.Team, error) {
	var t accounts.Team
	err := s.db.QueryRow(ctx, "SELECT id, org_id, slug, name, created_at FROM teams WHERE "+where, args...).Scan(&t.ID, &t.OrgID, &t.Slug, &t.Name, &t.CreatedAt)
	t.CreatedAt = t.CreatedAt.UTC()
	return t, mapErr(err, "team")
}

// TeamByID implements accounts.Store.
func (s *Store) TeamByID(ctx context.Context, id string) (accounts.Team, error) {
	return s.team(ctx, "id = $1", id)
}

// TeamBySlug implements accounts.Store.
func (s *Store) TeamBySlug(ctx context.Context, orgID, slug string) (accounts.Team, error) {
	return s.team(ctx, "org_id = $1 AND slug = $2", orgID, slug)
}

// OrgTeams implements accounts.Store.
func (s *Store) OrgTeams(ctx context.Context, orgID string) ([]accounts.Team, error) {
	rows, err := s.db.Query(ctx, "SELECT id, org_id, slug, name, created_at FROM teams WHERE org_id = $1 ORDER BY name, id", orgID)
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

// DeleteTeam implements accounts.Store: the team and every tuple naming it,
// in one transaction.
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

func (s *Store) deleteTeam(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, "DELETE FROM teams WHERE id = $1", id)
		if err := affected(tag, err, "team"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM relation_tuples WHERE (object_type = 'team' AND object_id = $1) OR (subject_type = 'team' AND subject_id = $1)", id)
		return err
	})
}

const tupleCols = "object_type, object_id, relation, subject_type, subject_id, subject_relation, created_by, created_at"

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

func (s *Store) writeTuple(ctx context.Context, t accounts.Tuple) error {
	_, err := s.db.Exec(ctx, "INSERT INTO relation_tuples ("+tupleCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		t.ObjectType, t.ObjectID, t.Relation, t.SubjectType, t.SubjectID, t.SubjectRelation, t.CreatedBy, t.CreatedAt)
	return mapErr(err, "tuple")
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

func (s *Store) deleteTuple(ctx context.Context, t accounts.Tuple) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM relation_tuples WHERE object_type = $1 AND object_id = $2 AND relation = $3
  AND subject_type = $4 AND subject_id = $5 AND subject_relation = $6`, t.ObjectType, t.ObjectID, t.Relation, t.SubjectType, t.SubjectID, t.SubjectRelation)
	return affected(tag, err, "tuple")
}

// tupleWhere builds a parameterised condition: column names are fixed
// here, values are parameters.
func tupleWhere(f accounts.TupleFilter) (string, []any) {
	conds := []string{"true"}
	var args []any
	for _, c := range []struct{ col, val string }{
		{"object_type", f.ObjectType}, {"object_id", f.ObjectID}, {"relation", f.Relation},
		{"subject_type", f.SubjectType}, {"subject_id", f.SubjectID}, {"subject_relation", f.SubjectRelation},
	} {
		if c.val != "" {
			args = append(args, c.val)
			conds = append(conds, fmt.Sprintf("%s = $%d", c.col, len(args)))
		}
	}
	if f.ObjectIDs != nil {
		args = append(args, f.ObjectIDs)
		conds = append(conds, fmt.Sprintf("object_id = ANY($%d)", len(args)))
	}
	if f.SubjectIDs != nil {
		args = append(args, f.SubjectIDs)
		conds = append(conds, fmt.Sprintf("subject_id = ANY($%d)", len(args)))
	}
	return strings.Join(conds, " AND "), args
}

// ReadTuples implements accounts.Store.
func (s *Store) ReadTuples(ctx context.Context, f accounts.TupleFilter) ([]accounts.Tuple, error) {
	where, args := tupleWhere(f)
	rows, err := s.db.Query(ctx, "SELECT "+tupleCols+" FROM relation_tuples WHERE "+where+
		fmt.Sprintf(" ORDER BY object_type, object_id, relation, subject_type, subject_id, subject_relation LIMIT %d", accounts.MaxTuples+1), args...)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Tuple, error) {
		var t accounts.Tuple
		err := r.Scan(&t.ObjectType, &t.ObjectID, &t.Relation, &t.SubjectType, &t.SubjectID, &t.SubjectRelation, &t.CreatedBy, &t.CreatedAt)
		t.CreatedAt = t.CreatedAt.UTC()
		return t, err
	})
	if len(out) > accounts.MaxTuples {
		return nil, accounts.ErrAccessLimit
	}
	return out, err
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

func (s *Store) deleteTuples(ctx context.Context, f accounts.TupleFilter) (int, error) {
	where, args := tupleWhere(f)
	tag, err := s.db.Exec(ctx, "DELETE FROM relation_tuples WHERE "+where, args...)
	return int(tag.RowsAffected()), err
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

func (s *Store) setUserDisabled(ctx context.Context, userID string, at *time.Time) error {
	tag, err := s.db.Exec(ctx, "UPDATE users SET disabled_at = $2 WHERE id = $1", userID, at)
	return affected(tag, err, "user")
}

// SetPlatformAdmin implements accounts.Store.
func (s *Store) SetPlatformAdmin(ctx context.Context, userID string, admin bool) error {
	tag, err := s.db.Exec(ctx, "UPDATE users SET platform_admin = $2 WHERE id = $1", userID, admin)
	return affected(tag, err, "user")
}

func scanRepo(r pgx.CollectableRow) (accounts.Repository, error) {
	var x accounts.Repository
	err := r.Scan(&x.OrgID, &x.Graph, &x.Remote, &x.Visibility, &x.AddedBy, &x.AddedAt, &x.Access, &x.VerifiedAt)
	x.AddedAt = x.AddedAt.UTC()
	if x.VerifiedAt != nil {
		v := x.VerifiedAt.UTC()
		x.VerifiedAt = &v
	}
	return x, err

}
