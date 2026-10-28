package accounts

import (
	"context"
	"errors"
	"fmt"
)

// A person's own Git tokens read the repositories in their own space, and
// nothing else: organisation repositories use the organisation's token, and
// a personal repository never falls back to it, so a member cannot pull
// code the organisation's token reads into a space its admins do not see.
// Only the person manages their tokens, in a signed-in session; agents
// (API keys) never see or change them.

// userTokenPurpose binds a sealed token to a person instead of an
// organisation, so a row copied between the two tables does not open.
const userTokenPurpose = "user-git-token"

// selfOnly allows a person's own signed-in, business-capable session.
func (s *Service) selfOnly(ctx context.Context, p Principal) (User, error) {
	if err := signedIn(p); err != nil {
		return User{}, err
	}
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil {
		return User{}, ErrForbidden
	}
	if u.DisabledAt != nil {
		return User{}, ErrForbidden
	}
	if err := businessAllowed(ctx, s.St, u); err != nil {
		return User{}, err
	}
	return u, nil
}

// SetUserGitToken seals and stores the caller's own token for host. The
// change is audited first; a failed audit refuses it.
func (s *Service) SetUserGitToken(ctx context.Context, p Principal, host, token string) (UserGitToken, error) {
	if s.Secrets == nil {
		return UserGitToken{}, ErrNoSecretKey
	}
	u, err := s.selfOnly(ctx, p)
	if err != nil {
		return UserGitToken{}, err
	}
	if host, err = NormalizeHost(host); err != nil {
		return UserGitToken{}, err
	}
	if token, err = checkToken(token); err != nil {
		return UserGitToken{}, err
	}
	sealed, err := s.Secrets.SealFor(userTokenPurpose, token, u.ID, host)
	if err != nil {
		return UserGitToken{}, err
	}
	now := s.now()
	t := UserGitToken{UserID: u.ID, Host: host, Preview: Preview(token), KeyID: s.Secrets.KeyID, Sealed: sealed, CreatedAt: now, UpdatedAt: now}
	if old, err := s.St.UserGitToken(ctx, u.ID, host); err == nil {
		t.CreatedAt = old.CreatedAt
	} else if !errors.Is(err, ErrNotFound) {
		return UserGitToken{}, err
	}
	if _, err := s.Record(ctx, actorEvent(p, "user.git_token.set", "", host)); err != nil {
		return UserGitToken{}, err
	}
	if err := s.St.PutUserGitToken(ctx, t); err != nil {
		return UserGitToken{}, err
	}
	return t, nil
}

// RemoveUserGitToken deletes the caller's own token for host (audited first).
func (s *Service) RemoveUserGitToken(ctx context.Context, p Principal, host string) error {
	u, err := s.selfOnly(ctx, p)
	if err != nil {
		return err
	}
	if host, err = NormalizeHost(host); err != nil {
		return err
	}
	if _, err := s.St.UserGitToken(ctx, u.ID, host); err != nil {
		return err
	}
	if _, err := s.Record(ctx, actorEvent(p, "user.git_token.remove", "", host)); err != nil {
		return err
	}
	return s.St.DeleteUserGitToken(ctx, u.ID, host)
}

// UserGitTokens lists the caller's own tokens (host and preview).
func (s *Service) UserGitTokens(ctx context.Context, p Principal) ([]UserGitToken, error) {
	u, err := s.selfOnly(ctx, p)
	if err != nil {
		return nil, err
	}
	return s.St.UserGitTokens(ctx, u.ID)
}

// OpenUserGitToken returns userID's token for host in clear, for a clone,
// fetch or push of their own repository in orgID. The person must still be
// an active member of orgID; otherwise, as without a token, ErrNotFound.
func (s *Service) OpenUserGitToken(ctx context.Context, orgID, userID, host string) (string, error) {
	if s.Secrets == nil {
		return "", ErrNoSecretKey
	}
	gone := fmt.Errorf("git token: %w", ErrNotFound)
	u, err := s.St.UserByID(ctx, userID)
	if err != nil || u.DisabledAt != nil || businessAllowed(ctx, s.St, u) != nil {
		return "", gone
	}
	if _, err := s.St.MemberRole(ctx, orgID, userID); err != nil {
		return "", gone
	}
	t, err := s.St.UserGitToken(ctx, userID, host)
	if err != nil {
		return "", err
	}
	return s.Secrets.OpenFor(userTokenPurpose, t.Sealed, t.KeyID, userID, host)
}
