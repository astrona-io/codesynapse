package accounts

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var hostRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// NormalizeHost lowercases a Git host and rejects anything but a DNS name
// (no scheme, port, path or credentials). Callers that clone also apply
// gitx.ValidateRemote, which rejects internal addresses.
func NormalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if len(h) > 253 || !hostRe.MatchString(h) {
		return "", fmt.Errorf("%w: a host name such as github.com is needed", ErrInvalidInput)
	}
	return h, nil
}

func checkToken(t string) (string, error) {
	t = strings.TrimSpace(t)
	if len(t) < 8 || len(t) > 512 {
		return "", fmt.Errorf("%w: a token is 8 to 512 characters", ErrInvalidInput)
	}
	for _, r := range t {
		if r > unicode.MaxASCII || unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("%w: a token is printable ASCII without spaces", ErrInvalidInput)
		}
	}
	return t, nil
}

// SetGitToken seals and stores an organisation's token for host. Owners
// and admins only; never with an API key (agents do not manage secrets).
func (s *Service) SetGitToken(ctx context.Context, p Principal, orgID, host, token string) (GitToken, error) {
	if s.Secrets == nil {
		return GitToken{}, ErrNoSecretKey
	}
	if p.Via == "key" {
		return GitToken{}, ErrForbidden
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return GitToken{}, err
	}
	host, err := NormalizeHost(host)
	if err != nil {
		return GitToken{}, err
	}
	if token, err = checkToken(token); err != nil {
		return GitToken{}, err
	}
	sealed, err := s.Secrets.Seal(token, orgID, host)
	if err != nil {
		return GitToken{}, err
	}
	t := GitToken{OrgID: orgID, Host: host, Preview: Preview(token), KeyID: s.Secrets.KeyID, Sealed: sealed, CreatedBy: p.User.ID, UpdatedAt: s.now()}
	if err := s.St.PutGitToken(ctx, t); err != nil {
		return GitToken{}, err
	}
	s.audit(ctx, actorEvent(p, "org.git_token.set", orgID, host))
	return t, nil
}

// RemoveGitToken deletes an organisation's token for host.
func (s *Service) RemoveGitToken(ctx context.Context, p Principal, orgID, host string) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	host, err := NormalizeHost(host)
	if err != nil {
		return err
	}
	if err := s.St.DeleteGitToken(ctx, orgID, host); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "org.git_token.remove", orgID, host))
	return nil
}

// GitTokens lists an organisation's tokens (host and preview) for members.
func (s *Service) GitTokens(ctx context.Context, p Principal, orgID string) ([]GitToken, error) {
	if err := s.Require(ctx, p, orgID, Member); err != nil {
		return nil, err
	}
	return s.St.GitTokens(ctx, orgID)
}

// OpenGitToken returns an organisation's token for host in clear, for a
// clone or fetch the caller is about to run. ErrNotFound without one.
func (s *Service) OpenGitToken(ctx context.Context, orgID, host string) (string, error) {
	if s.Secrets == nil {
		return "", ErrNoSecretKey
	}
	t, err := s.St.GitToken(ctx, orgID, host)
	if err != nil {
		return "", err
	}
	return s.Secrets.Open(t.Sealed, t.KeyID, orgID, host)
}
