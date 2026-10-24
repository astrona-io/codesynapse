package accounts

import (
	"context"
	"errors"
	"fmt"
)

// The first start of an empty installation creates an administrator so the
// operator can sign in at once. It is the bootstrap root (bootstrap.go) with
// a generated password that is shown once, never a fixed one, and the
// dashboard tells whoever signs in with it to replace it (ReplaceRoot).

// Names of the account made at first start.
const (
	InitialAdminUsername = "admin"
	InitialAdminEmail    = "admin@codesynapse.local"
)

// ErrNotAutoCreated reports an attempt to replace a root the operator made.
var ErrNotAutoCreated = fmt.Errorf("%w: only the administrator made at first start can be replaced", ErrForbidden)

// EnsureInitialAdmin creates the initial administrator when the installation
// has no accounts. password is used when set (and must meet the password
// policy); otherwise one is generated. generated is the password to show the
// operator once, and is empty when nothing was created or the operator
// supplied it. Concurrent starts create at most one account.
func (s *Service) EnsureInitialAdmin(ctx context.Context, password string) (u User, generated string, err error) {
	n, err := s.St.CountUsers(ctx)
	if err != nil || n > 0 {
		return User{}, "", err
	}
	if password == "" {
		if generated, err = randomToken(18); err != nil {
			return User{}, "", err
		}
		password = generated
	}
	u, err = s.newAccount(InitialAdminEmail, InitialAdminUsername, "Administrator", password)
	if err != nil {
		return User{}, "", err
	}
	u.AutoCreated = true
	u, err = s.signUpRoot(ctx, u)
	if errors.Is(err, errNotFirst) {
		return User{}, "", nil // another replica created it
	}
	if err != nil {
		return User{}, "", err
	}
	return u, generated, nil
}

// InitialAdminActive reports whether the account made at first start still
// exists, so the sign-in page can say where its password is.
func (s *Service) InitialAdminActive(ctx context.Context) (bool, error) {
	setup, err := s.St.Setup(ctx)
	if err != nil || setup.RootUserID == "" {
		return false, err
	}
	root, err := s.St.UserByID(ctx, setup.RootUserID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil && root.AutoCreated, err
}

// ReplaceRoot creates the caller's own root account and deletes the one made
// at first start, in one transaction, then ends every session of the old
// account. The new account is root and platform administrator; the setup
// record follows it. Only a signed-in session of the auto-created root may do
// this, and the result needs a fresh sign-in.
func (s *Service) ReplaceRoot(ctx context.Context, p Principal, in SignUpInput) (User, error) {
	if err := signedIn(p); err != nil {
		return User{}, err
	}
	nu, err := s.newAccount(in.Email, in.Username, in.Name, in.Password)
	if err != nil {
		return User{}, err
	}
	nu.PlatformAdmin = true
	return policyChange(ctx, s, func(tx *Service) (User, error) {
		old, err := tx.St.UserByID(ctx, p.User.ID)
		if err != nil || old.DisabledAt != nil {
			return User{}, ErrUnauthorized
		}
		if !old.BootstrapRoot {
			return User{}, ErrForbidden
		}
		if !old.AutoCreated {
			return User{}, ErrNotAutoCreated
		}
		if err := tx.ValidatePrincipal(ctx, p); err != nil {
			return User{}, err
		}
		if err := tx.St.ReplaceRoot(ctx, old.ID, nu); err != nil {
			return User{}, err
		}
		e := actorEvent(p, "setup.root_replaced", "", nu.Email)
		e.Outcome = Success
		tx.audit(ctx, e)
		nu.BootstrapRoot = true
		return nu, nil
	})
}

// ResetRoot is the operator's way back in (codesynapse-api reset-admin): it
// sets the root's password, lifts a suspension and ends its sessions. With
// no accounts it creates the initial administrator instead. password is
// used when set, otherwise one is generated and returned for the operator to
// see once. The root's name, email and username are unchanged.
func (s *Service) ResetRoot(ctx context.Context, password string) (u User, generated string, err error) {
	if u, generated, err = s.EnsureInitialAdmin(ctx, password); err != nil || u.ID != "" {
		return u, generated, err
	}
	setup, err := s.St.Setup(ctx)
	if err != nil {
		return User{}, "", err
	}
	if setup.RootUserID == "" {
		return User{}, "", fmt.Errorf("%w: this installation has no identified root; run designate-root first", ErrNotFound)
	}
	if password == "" {
		if generated, err = randomToken(18); err != nil {
			return User{}, "", err
		}
		password = generated
	}
	if err := CheckPassword(password); err != nil {
		return User{}, "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, "", err
	}
	u, err = s.St.UserByID(ctx, setup.RootUserID)
	if err != nil {
		return User{}, "", err
	}
	if err := s.St.SetPassword(ctx, u.ID, hash); err != nil {
		return User{}, "", err
	}
	if err := s.St.SetUserDisabled(ctx, u.ID, nil); err != nil {
		return User{}, "", err
	}
	if err := s.St.DeleteUserSessions(ctx, u.ID); err != nil {
		return User{}, "", err
	}
	s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "operator", Action: "setup.root_reset", Target: u.Email, Outcome: Success})
	u.PasswordHash = ""
	return u, generated, nil
}
