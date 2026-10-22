package accounts

import (
	"context"
	"errors"
	"fmt"
)

// Platform administration: suspending accounts and granting the platform
// administrator flag. Only platform admins, never through an API key, and
// never in a way that locks everyone out.

func (s *Service) requirePlatform(p Principal) error {
	if !p.User.PlatformAdmin || p.Via == "key" {
		return ErrForbidden
	}
	return nil
}

// SuspendUser stops a user: no sign-in, sessions ended, API keys refused.
// Their memberships, keys and history stay, so ResumeUser restores them.
func (s *Service) SuspendUser(ctx context.Context, p Principal, userID string) error {
	if err := s.requirePlatform(p); err != nil {
		return err
	}
	if userID == p.User.ID {
		return fmt.Errorf("%w: you cannot suspend yourself", ErrInvalidInput)
	}
	u, err := s.St.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.PlatformAdmin {
		if err := s.keepAnAdmin(ctx, userID); err != nil {
			return err
		}
	}
	now := s.now()
	if err := s.St.SetUserDisabled(ctx, userID, &now); err != nil {
		return err
	}
	if err := s.St.DeleteUserSessions(ctx, userID); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "user.suspend", "", u.Email))
	return nil
}

// ResumeUser lifts a suspension.
func (s *Service) ResumeUser(ctx context.Context, p Principal, userID string) error {
	if err := s.requirePlatform(p); err != nil {
		return err
	}
	u, err := s.St.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.St.SetUserDisabled(ctx, userID, nil); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "user.resume", "", u.Email))
	return nil
}

// SetPlatformAdmin grants or revokes the platform administrator flag. The
// last active platform admin cannot be removed.
func (s *Service) SetPlatformAdmin(ctx context.Context, p Principal, userID string, admin bool) error {
	if err := s.requirePlatform(p); err != nil {
		return err
	}
	u, err := s.St.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if !admin && u.PlatformAdmin {
		if err := s.keepAnAdmin(ctx, userID); err != nil {
			return err
		}
	}
	if admin && u.DisabledAt != nil {
		return fmt.Errorf("%w: resume the account first", ErrInvalidInput)
	}
	if err := s.St.SetPlatformAdmin(ctx, userID, admin); err != nil {
		return err
	}
	action := "user.platform_admin.revoke"
	if admin {
		action = "user.platform_admin.grant"
	}
	s.audit(ctx, actorEvent(p, action, "", u.Email))
	return nil
}

// ErrLastAdmin keeps at least one active platform administrator.
var ErrLastAdmin = errors.New("the installation needs at least one active platform administrator")

func (s *Service) keepAnAdmin(ctx context.Context, leaving string) error {
	users, err := s.St.ListUsers(ctx, 100000)
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.ID != leaving && u.PlatformAdmin && u.DisabledAt == nil {
			return nil
		}
	}
	return ErrLastAdmin
}
