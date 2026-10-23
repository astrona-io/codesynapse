package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const idpCols = "org_id, name, issuer, client_id, secret_preview, key_id, sealed_secret, settings, enabled, created_by, updated_at"

// PutIdentityProvider implements accounts.Store.
func (s *Store) PutIdentityProvider(ctx context.Context, p accounts.IdentityProvider) error {
	settings, err := json.Marshal(p.IdentityProviderSettings)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO identity_providers (`+idpCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (org_id) DO UPDATE SET name = $2, issuer = $3, client_id = $4, secret_preview = $5, key_id = $6, sealed_secret = $7,
  settings = $8, enabled = $9, created_by = $10, updated_at = $11`,
		p.OrgID, p.Name, p.Issuer, p.ClientID, p.SecretPreview, p.KeyID, p.SealedSecret, settings, p.Enabled, p.CreatedBy, p.UpdatedAt)
	return mapErr(err, "identity provider")
}

// IdentityProvider implements accounts.Store.
func (s *Store) IdentityProvider(ctx context.Context, orgID string) (accounts.IdentityProvider, error) {
	var p accounts.IdentityProvider
	var settings []byte
	err := s.db.QueryRow(ctx, "SELECT "+idpCols+" FROM identity_providers WHERE org_id = $1", orgID).Scan(
		&p.OrgID, &p.Name, &p.Issuer, &p.ClientID, &p.SecretPreview, &p.KeyID, &p.SealedSecret, &settings, &p.Enabled, &p.CreatedBy, &p.UpdatedAt)
	if err != nil {
		return accounts.IdentityProvider{}, mapErr(err, "identity provider")
	}
	if err := json.Unmarshal(settings, &p.IdentityProviderSettings); err != nil {
		return accounts.IdentityProvider{}, fmt.Errorf("identity provider settings: %w", err)
	}
	p.UpdatedAt = p.UpdatedAt.UTC()
	return p, nil
}

// DeleteIdentityProvider implements accounts.Store.
func (s *Store) DeleteIdentityProvider(ctx context.Context, orgID string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM identity_providers WHERE org_id = $1", orgID)
	return affected(tag, err, "identity provider")
}

const identityCols = "id, org_id, issuer, subject, user_id, email, linked_at, last_login_at"

func scanIdentity(row pgx.Row) (accounts.ExternalIdentity, error) {
	var e accounts.ExternalIdentity
	err := row.Scan(&e.ID, &e.OrgID, &e.Issuer, &e.Subject, &e.UserID, &e.Email, &e.LinkedAt, &e.LastLoginAt)
	e.LinkedAt = e.LinkedAt.UTC()
	if e.LastLoginAt != nil {
		t := e.LastLoginAt.UTC()
		e.LastLoginAt = &t
	}
	return e, err
}

// IdentityBySubject implements accounts.Store.
func (s *Store) IdentityBySubject(ctx context.Context, issuer, subject string) (accounts.ExternalIdentity, error) {
	e, err := scanIdentity(s.db.QueryRow(ctx, "SELECT "+identityCols+" FROM external_identities WHERE issuer = $1 AND subject = $2", issuer, subject))
	return e, mapErr(err, "identity")
}

// PutIdentity implements accounts.Store.
func (s *Store) PutIdentity(ctx context.Context, e accounts.ExternalIdentity) error {
	_, err := s.db.Exec(ctx, `INSERT INTO external_identities (`+identityCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET email = $6, last_login_at = $8`,
		e.ID, e.OrgID, e.Issuer, e.Subject, e.UserID, e.Email, e.LinkedAt, e.LastLoginAt)
	return mapErr(err, "identity")
}

// UserIdentities implements accounts.Store.
func (s *Store) UserIdentities(ctx context.Context, userID string) ([]accounts.ExternalIdentity, error) {
	rows, err := s.db.Query(ctx, "SELECT "+identityCols+" FROM external_identities WHERE user_id = $1 ORDER BY id LIMIT 100", userID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.ExternalIdentity, error) { return scanIdentity(r) })
	if out == nil {
		out = []accounts.ExternalIdentity{}
	}
	return out, err
}

// DeleteIdentity implements accounts.Store.
func (s *Store) DeleteIdentity(ctx context.Context, userID, id string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM external_identities WHERE id = $1 AND user_id = $2", id, userID)
	return affected(tag, err, "identity")
}

// PutSSOState implements accounts.Store.
func (s *Store) PutSSOState(ctx context.Context, st accounts.SSOState) error {
	_, err := s.db.Exec(ctx, `INSERT INTO sso_states (state_hash, binder_hash, org_id, mode, user_id, nonce, verifier, next, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		st.StateHash, st.BinderHash, st.OrgID, st.Mode, nullable(st.UserID), st.Nonce, st.Verifier, st.Next, st.ExpiresAt)
	return mapErr(err, "sso state")
}

// TakeSSOState implements accounts.Store. DELETE … RETURNING makes the state
// usable once even under concurrent callbacks.
func (s *Store) TakeSSOState(ctx context.Context, stateHash string) (accounts.SSOState, error) {
	var st accounts.SSOState
	var userID *string
	err := s.db.QueryRow(ctx, `DELETE FROM sso_states WHERE state_hash = $1
RETURNING state_hash, binder_hash, org_id, mode, user_id, nonce, verifier, next, expires_at`, stateHash).Scan(
		&st.StateHash, &st.BinderHash, &st.OrgID, &st.Mode, &userID, &st.Nonce, &st.Verifier, &st.Next, &st.ExpiresAt)
	if err != nil {
		return accounts.SSOState{}, mapErr(err, "sso state")
	}
	st.UserID, st.ExpiresAt = deref(userID), st.ExpiresAt.UTC()
	return st, nil
}

// PruneSSOStates implements accounts.Store.
func (s *Store) PruneSSOStates(ctx context.Context, now time.Time) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sso_states WHERE expires_at <= $1", now)
	return err
}

// ManagedByIDP implements accounts.Store.
func (s *Store) ManagedByIDP(ctx context.Context, orgID, userID string) (bool, error) {
	var managed bool
	err := s.db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM idp_memberships WHERE org_id = $1 AND user_id = $2)", orgID, userID).Scan(&managed)
	return managed, err
}

// SetManagedByIDP implements accounts.Store.
func (s *Store) SetManagedByIDP(ctx context.Context, orgID, userID string, managed bool) error {
	if !managed {
		_, err := s.db.Exec(ctx, "DELETE FROM idp_memberships WHERE org_id = $1 AND user_id = $2", orgID, userID)
		return err
	}
	_, err := s.db.Exec(ctx, "INSERT INTO idp_memberships (org_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", orgID, userID)
	return mapErr(err, "membership")
}
