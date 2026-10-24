package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// A person's own account: name, email and the sessions they are signed in
// with. Every change needs a signed-in session (never an API key, which
// acts for an agent) and is audited.

// ErrPasswordless reports an account created through single sign-on: it has
// no password to confirm a change with, and its email belongs to the
// identity provider.
var ErrPasswordless = fmt.Errorf("%w: this account signs in with single sign-on and has no password; change the email at your identity provider", ErrForbidden)

// SessionView is one of a person's sessions as they may see it.
type SessionView struct {
	// ID is derived from the session's hash; it names the session to end
	// without being the key that looks it up.
	ID        string    `json:"id"`
	Device    string    `json:"device"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

func sessionID(hash string) string {
	sum := sha256.Sum256([]byte("session-view:" + hash))
	return hex.EncodeToString(sum[:8])
}

func signedIn(p Principal) error {
	if p.Via != "session" || p.SessionHash == "" {
		return fmt.Errorf("%w: change your account in a signed-in browser session", ErrForbidden)
	}
	return nil
}

// UpdateName changes the caller's display name.
func (s *Service) UpdateName(ctx context.Context, p Principal, name string) (User, error) {
	if err := signedIn(p); err != nil {
		return User{}, err
	}
	name, err := checkName(clipText(name, 200), "name")
	if err != nil {
		return User{}, err
	}
	if err := s.St.SetUserName(ctx, p.User.ID, name); err != nil {
		return User{}, err
	}
	s.audit(ctx, actorEvent(p, "user.profile", "", p.User.Email))
	return s.St.UserByID(ctx, p.User.ID)
}

// ChangeEmail changes the caller's email after their password confirms it,
// and ends their other sessions: an email is how the account is found.
func (s *Service) ChangeEmail(ctx context.Context, p Principal, password, email string) (User, error) {
	if err := signedIn(p); err != nil {
		return User{}, err
	}
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil {
		return User{}, err
	}
	if u.PasswordHash == "" {
		return User{}, ErrPasswordless
	}
	if !VerifyPassword(password, u.PasswordHash) {
		s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: p.Via, Action: "user.email", Target: u.Email, Outcome: Failure, Detail: "wrong password"})
		return User{}, ErrUnauthorized
	}
	email = NormalizeEmail(email)
	if !emailRe.MatchString(email) {
		return User{}, fmt.Errorf("%w: that is not an email address", ErrInvalidInput)
	}
	if email == u.Email {
		return u, nil
	}
	if err := s.St.SetUserEmail(ctx, u.ID, email); err != nil {
		if errors.Is(err, ErrExists) {
			return User{}, fmt.Errorf("%w: another account uses that email", ErrExists)
		}
		return User{}, err
	}
	s.audit(ctx, actorEvent(p, "user.email", "", email))
	if err := s.St.DeleteOtherSessions(ctx, u.ID, p.SessionHash); err != nil {
		return User{}, err
	}
	return s.St.UserByID(ctx, u.ID)
}

// Sessions lists the caller's sessions, the current one marked.
func (s *Service) Sessions(ctx context.Context, p Principal) ([]SessionView, error) {
	if err := signedIn(p); err != nil {
		return nil, err
	}
	list, err := s.St.UserSessions(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}
	out := make([]SessionView, 0, len(list))
	for _, sess := range list {
		out = append(out, SessionView{ID: sessionID(sess.TokenHash), Device: clipText(sess.UserAgent, 200), CreatedAt: sess.CreatedAt,
			ExpiresAt: sess.ExpiresAt, Current: sess.TokenHash == p.SessionHash})
	}
	return out, nil
}

// EndSession ends one of the caller's sessions by its view ID.
func (s *Service) EndSession(ctx context.Context, p Principal, id string) error {
	if err := signedIn(p); err != nil {
		return err
	}
	list, err := s.St.UserSessions(ctx, p.User.ID)
	if err != nil {
		return err
	}
	for _, sess := range list {
		if sessionID(sess.TokenHash) == id {
			if err := s.St.DeleteSession(ctx, sess.TokenHash); err != nil {
				return err
			}
			s.audit(ctx, actorEvent(p, "user.session_end", "", p.User.Email))
			return nil
		}
	}
	return fmt.Errorf("session: %w", ErrNotFound)
}

// EndOtherSessions signs the caller out everywhere but here.
func (s *Service) EndOtherSessions(ctx context.Context, p Principal) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.St.DeleteOtherSessions(ctx, p.User.ID, p.SessionHash); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "user.sessions_end_others", "", p.User.Email))
	return nil
}

// clipText drops control characters and keeps at most n runes.
func clipText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
