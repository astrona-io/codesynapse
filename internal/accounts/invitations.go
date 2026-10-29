package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Invitations add people to an organisation by email without telling the
// inviter whether that email has an account: creating one never looks a user
// up, and every invitation answers the same. There is no mail delivery, so
// the inviter gets a one-time link to pass on. The invited person accepts it
// in a signed-in session of the account with that email; the link alone or
// the email alone is not enough. Only the token's SHA-256 is stored.

// Invitation is a pending offer of a role in an organisation.
type Invitation struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// Email is the invited address, normalised (NormalizeEmail).
	Email string `json:"email"`
	// Role is the role offered. Accepting grants at most the inviter's
	// role at that moment.
	Role      Role      `json:"role"`
	InvitedBy string    `json:"invited_by"` // user ID
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// TokenHash is HashToken of the accept token; never serialised.
	TokenHash string `json:"-"`
}

// InvitationPreview is what the accept page shows the token's holder.
type InvitationPreview struct {
	Org       Org       `json:"org"`
	Role      Role      `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
	// ForYou says the signed-in account's email is the invited one.
	ForYou bool `json:"for_you"`
}

const (
	// InvitationTTL is how long an invitation can be accepted.
	InvitationTTL = 7 * 24 * time.Hour
	// MaxPendingInvitations bounds the open invitations of one organisation.
	MaxPendingInvitations = 50
	// maxInviteToken bounds a presented token before it is hashed.
	maxInviteToken = 128
)

var (
	// ErrInvitationInvalid covers unknown, used, revoked and expired
	// tokens alike.
	ErrInvitationInvalid = fmt.Errorf("invitation %w: it may have expired, been used or been revoked; ask for a new one", ErrNotFound)
	// ErrInvitationEmail refuses an account whose email is not the invited one.
	ErrInvitationEmail = fmt.Errorf("%w: this invitation is for a different email address; sign in with the account it was sent to", ErrForbidden)
	// ErrInviterGone refuses an invitation whose inviter can no longer add members.
	ErrInviterGone = fmt.Errorf("%w: the person who invited you can no longer add members; ask for a new invitation", ErrForbidden)
	// ErrInvitationLimit refuses a new invitation beyond MaxPendingInvitations.
	ErrInvitationLimit = fmt.Errorf("%w: at most %d invitations can be pending; revoke some first", ErrExists, MaxPendingInvitations)
)

// inviteTx runs fn in one store transaction with audit entries written in
// it, so a failed audit refuses the change. Unlike policyChange it does not
// bump the access policy: pending invitations grant nothing.
func (s *Service) inviteTx(ctx context.Context, fn func(tx *Service) error) error {
	var events []Event
	err := s.St.PolicyTransaction(ctx, "", func(st Store) error {
		scoped := *s
		scoped.St = st
		scoped.Sink = func(e Event) { events = append(events, e) }
		return fn(&scoped)
	})
	if err == nil && s.Sink != nil {
		for _, e := range events {
			s.Sink(e)
		}
	}
	return err
}

// CreateInvitation offers role in orgID to email. Owners and admins create
// invitations in a signed-in session; nobody can offer a role above their
// own. The answer never depends on whether email has an account, and no
// account is looked up. The token is returned once.
func (s *Service) CreateInvitation(ctx context.Context, p Principal, orgID, email string, role Role) (Invitation, string, error) {
	if err := signedIn(p); err != nil {
		return Invitation{}, "", err
	}
	email = NormalizeEmail(email)
	if !emailRe.MatchString(email) {
		return Invitation{}, "", fmt.Errorf("%w: that is not an email address", ErrInvalidInput)
	}
	if !role.Valid() {
		return Invitation{}, "", fmt.Errorf("%w: unknown role %q", ErrInvalidInput, role)
	}
	token, err := randomToken(32)
	if err != nil {
		return Invitation{}, "", err
	}
	id, err := newID()
	if err != nil {
		return Invitation{}, "", err
	}
	var inv Invitation
	err = s.inviteTx(ctx, func(tx *Service) error {
		mine, err := tx.RoleIn(ctx, p, orgID)
		if err != nil {
			return err
		}
		if !mine.AtLeast(Admin) || !mine.AtLeast(role) {
			return ErrForbidden
		}
		open, err := tx.expireInvitations(ctx, orgID)
		if err != nil {
			return err
		}
		if len(open) >= MaxPendingInvitations {
			return ErrInvitationLimit
		}
		now := tx.now()
		inv = Invitation{ID: id, OrgID: orgID, Email: email, Role: role, InvitedBy: p.User.ID, CreatedAt: now, ExpiresAt: now.Add(InvitationTTL), TokenHash: HashToken(token)}
		e := actorEvent(p, "org.invite.create", orgID, email)
		e.Detail = "invitation " + id + " role " + string(role)
		if _, err := tx.Record(ctx, e); err != nil {
			return err
		}
		return tx.St.PutInvitation(ctx, inv)
	})
	if err != nil {
		return Invitation{}, "", err
	}
	return inv, token, nil
}

// expireInvitations removes orgID's expired invitations, each audited
// first, and returns the ones still open.
func (s *Service) expireInvitations(ctx context.Context, orgID string) ([]Invitation, error) {
	all, err := s.St.OrgInvitations(ctx, orgID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	open := all[:0]
	for _, inv := range all {
		if inv.ExpiresAt.After(now) {
			open = append(open, inv)
			continue
		}
		if err := s.expire(ctx, inv); err != nil {
			return nil, err
		}
	}
	return open, nil
}

func (s *Service) expire(ctx context.Context, inv Invitation) error {
	e := Event{Via: "system", OrgID: inv.OrgID, Action: "org.invite.expire", Target: inv.Email, Detail: "invitation " + inv.ID}
	if _, err := s.Record(ctx, e); err != nil {
		return err
	}
	return s.St.DeleteInvitation(ctx, inv.OrgID, inv.ID)
}

// Invitations lists orgID's open invitations for its owners and admins.
func (s *Service) Invitations(ctx context.Context, p Principal, orgID string) ([]Invitation, error) {
	// Pending invitations name people by email: sessions only, like creating them.
	if err := signedIn(p); err != nil {
		return nil, err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return nil, err
	}
	all, err := s.St.OrgInvitations(ctx, orgID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	open := []Invitation{}
	for _, inv := range all {
		if inv.ExpiresAt.After(now) {
			open = append(open, inv)
		}
	}
	return open, nil
}

// RevokeInvitation withdraws an open invitation (owners and admins, in a
// signed-in session; audited first).
func (s *Service) RevokeInvitation(ctx context.Context, p Principal, orgID, id string) error {
	if err := signedIn(p); err != nil {
		return err
	}
	return s.inviteTx(ctx, func(tx *Service) error {
		if err := tx.Require(ctx, p, orgID, Admin); err != nil {
			return err
		}
		open, err := tx.expireInvitations(ctx, orgID)
		if err != nil {
			return err
		}
		for _, inv := range open {
			if inv.ID != id {
				continue
			}
			e := actorEvent(p, "org.invite.revoke", orgID, inv.Email)
			e.Detail = "invitation " + inv.ID
			if _, err := tx.Record(ctx, e); err != nil {
				return err
			}
			return tx.St.DeleteInvitation(ctx, orgID, inv.ID)
		}
		return fmt.Errorf("invitation: %w", ErrNotFound)
	})
}

// openInvitation finds the open invitation for token. An expired one is
// removed (audited) in its own transaction and reported like any other
// invalid token.
func (s *Service) openInvitation(ctx context.Context, token string) (Invitation, error) {
	if token == "" || len(token) > maxInviteToken {
		return Invitation{}, ErrInvitationInvalid
	}
	inv, err := s.St.InvitationByTokenHash(ctx, HashToken(token))
	if errors.Is(err, ErrNotFound) {
		return Invitation{}, ErrInvitationInvalid
	}
	if err != nil {
		return Invitation{}, err
	}
	if !inv.ExpiresAt.After(s.now()) {
		err := s.inviteTx(ctx, func(tx *Service) error {
			if _, err := tx.St.InvitationByTokenHash(ctx, inv.TokenHash); err != nil {
				return nil //nolint:nilerr // already gone: nothing to expire
			}
			return tx.expire(ctx, inv)
		})
		if err != nil {
			return Invitation{}, err
		}
		return Invitation{}, ErrInvitationInvalid
	}
	return inv, nil
}

// invitee checks that p may accept invitations at all: a signed-in, active
// account that may do organisation work (never the bootstrap root).
func (s *Service) invitee(ctx context.Context, p Principal) (User, error) {
	if err := signedIn(p); err != nil {
		return User{}, err
	}
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		return User{}, err
	}
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil {
		return User{}, ErrUnauthorized
	}
	if u.DisabledAt != nil {
		return User{}, ErrSuspended
	}
	if err := businessAllowed(ctx, s.St, u); err != nil {
		return User{}, err
	}
	return u, nil
}

// PreviewInvitation tells the token's holder which organisation and role it
// offers and whether it is for their account. It changes nothing.
func (s *Service) PreviewInvitation(ctx context.Context, p Principal, token string) (InvitationPreview, error) {
	u, err := s.invitee(ctx, p)
	if err != nil {
		return InvitationPreview{}, err
	}
	inv, err := s.openInvitation(ctx, token)
	if err != nil {
		return InvitationPreview{}, err
	}
	o, err := s.St.OrgByID(ctx, inv.OrgID)
	if err != nil {
		return InvitationPreview{}, ErrInvitationInvalid
	}
	return InvitationPreview{Org: o, Role: inv.Role, ExpiresAt: inv.ExpiresAt, ForYou: NormalizeEmail(u.Email) == inv.Email}, nil
}

// AcceptInvitation makes the signed-in caller a member with the invited
// role, capped at the inviter's role now. The caller's account email must be
// the invited one; root and suspended accounts never accept. The invitation
// works once: it is removed in the same transaction that adds the member,
// after the acceptance is audited.
func (s *Service) AcceptInvitation(ctx context.Context, p Principal, token string) (Membership, error) {
	if _, err := s.invitee(ctx, p); err != nil {
		return Membership{}, err
	}
	inv, err := s.openInvitation(ctx, token)
	if err != nil {
		return Membership{}, err
	}
	m, err := policyChange(ctx, s, func(tx *Service) (Membership, error) { return tx.acceptPolicy(ctx, p, inv.TokenHash) })
	if errors.Is(err, ErrInvitationEmail) || errors.Is(err, ErrInviterGone) || errors.Is(err, ErrExists) {
		e := actorEvent(p, "org.invite.accept", inv.OrgID, p.User.Email)
		e.Outcome, e.Detail = Failure, "invitation "+inv.ID+": "+err.Error()
		s.audit(ctx, e)
	}
	return m, err
}

func (s *Service) acceptPolicy(ctx context.Context, p Principal, tokenHash string) (Membership, error) {
	u, err := s.invitee(ctx, p)
	if err != nil {
		return Membership{}, err
	}
	inv, err := s.St.InvitationByTokenHash(ctx, tokenHash)
	if errors.Is(err, ErrNotFound) || (err == nil && !inv.ExpiresAt.After(s.now())) {
		return Membership{}, ErrInvitationInvalid
	}
	if err != nil {
		return Membership{}, err
	}
	if NormalizeEmail(u.Email) != inv.Email {
		return Membership{}, ErrInvitationEmail
	}
	if _, err := s.St.MemberRole(ctx, inv.OrgID, u.ID); err == nil {
		return Membership{}, fmt.Errorf("%w: you are already a member of this organisation", ErrExists)
	} else if !errors.Is(err, ErrNotFound) {
		return Membership{}, err
	}
	ceiling, err := s.inviterRole(ctx, inv)
	if err != nil {
		return Membership{}, err
	}
	role := inv.Role
	if !ceiling.AtLeast(role) {
		role = ceiling
	}
	e := actorEvent(p, "org.invite.accept", inv.OrgID, inv.Email)
	e.Detail = "invitation " + inv.ID + " role " + string(role)
	if role != inv.Role {
		e.Detail += " (invited as " + string(inv.Role) + ", capped at the inviter's role)"
	}
	s.audit(ctx, e)
	if s.policyErr != nil {
		return Membership{}, s.policyErr
	}
	if err := s.St.DeleteInvitation(ctx, inv.OrgID, inv.ID); err != nil {
		return Membership{}, err
	}
	if err := s.St.SetMember(ctx, inv.OrgID, u.ID, role); err != nil {
		return Membership{}, err
	}
	o, err := s.St.OrgByID(ctx, inv.OrgID)
	return Membership{Org: o, User: u, Role: role, AddedAt: s.now()}, err
}

// inviterRole is the inviter's organisation role now; ErrInviterGone when
// they can no longer add members.
func (s *Service) inviterRole(ctx context.Context, inv Invitation) (Role, error) {
	iu, err := s.St.UserByID(ctx, inv.InvitedBy)
	if err != nil || iu.DisabledAt != nil || businessAllowed(ctx, s.St, iu) != nil {
		return "", ErrInviterGone
	}
	r, err := s.St.MemberRole(ctx, inv.OrgID, iu.ID)
	if errors.Is(err, ErrNotFound) {
		return "", ErrInviterGone
	}
	if err != nil {
		return "", err
	}
	if r != Owner {
		tr, err := s.teamOrgRole(ctx, inv.OrgID, iu.ID)
		if err != nil {
			return "", err
		}
		r = Max(r, tr)
	}
	if !r.AtLeast(Admin) {
		return "", ErrInviterGone
	}
	return r, nil
}
