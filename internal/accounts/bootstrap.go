package accounts

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The bootstrap root is the account that set up the installation. It keeps
// governance (settings, health, telemetry, audit, account suspension and
// recovery) and loses all organisation work: repositories, code, reviews,
// memory, keys and organisation settings, whatever memberships or grants it
// holds. Its only tenant change is the setup transaction that hands the
// installation to a standard owner. See docs/ROOT_BOOTSTRAP_PLAN.md.

// Setup states.
const (
	// SetupPending: root exists, no standard owner has been provisioned.
	SetupPending = "pending"
	// SetupComplete: an organisation and a standard owner exist.
	SetupComplete = "complete"
	// SetupAmbiguous: an existing installation whose root could not be
	// identified from its history. Every platform administrator is limited
	// to governance until an operator designates the root (DesignateRoot).
	SetupAmbiguous = "ambiguous"
)

// Setup is the installation's single bootstrap record.
type Setup struct {
	State       string     `json:"state"`
	RootUserID  string     `json:"root_user_id,omitempty"`
	OrgID       string     `json:"org_id,omitempty"`
	OwnerID     string     `json:"owner_id,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// SetupInput is what root submits to hand over the installation: a new
// organisation (OrgName) or, for an installation that predates bootstrap
// separation, an existing one (OrgID), and its standard owner.
type SetupInput struct {
	OrgName  string `json:"org_name"`
	OrgID    string `json:"org_id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// businessAllowed returns ErrRootAccount for an account limited to
// governance: the bootstrap root, and every platform administrator while the
// root of an existing installation is ambiguous. It reads the account as
// stored, so callers pass a freshly loaded user.
func businessAllowed(ctx context.Context, st Store, u User) error {
	if u.BootstrapRoot {
		return ErrRootAccount
	}
	if !u.PlatformAdmin {
		return nil
	}
	setup, err := st.Setup(ctx)
	if err != nil {
		return err
	}
	if setup.State == SetupAmbiguous {
		return ErrRootAccount
	}
	return nil
}

// Restricted reports whether the stored account may do governance only.
func (s *Service) Restricted(ctx context.Context, userID string) (bool, error) {
	u, err := s.St.UserByID(ctx, userID)
	if err != nil {
		return false, err
	}
	err = businessAllowed(ctx, s.St, u)
	if errors.Is(err, ErrRootAccount) {
		return true, nil
	}
	return false, err
}

// SetupState returns the installation's setup record.
func (s *Service) SetupState(ctx context.Context) (Setup, error) {
	return s.St.Setup(ctx)
}

// newAccount validates and prepares a standard account.
func (s *Service) newAccount(email, username, name, password string) (User, error) {
	email = NormalizeEmail(email)
	if !emailRe.MatchString(email) {
		return User{}, fmt.Errorf("%w: a valid email address is needed", ErrInvalidInput)
	}
	username, err := CheckUsername(username)
	if err != nil {
		return User{}, err
	}
	name, err = checkName(name, "a name")
	if err != nil {
		return User{}, err
	}
	if err := CheckPassword(password); err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Email: email, Username: username, Name: name, CreatedAt: s.now(), PasswordHash: hash}, nil
}

// maxAdminScan bounds account scans for installation-wide decisions.
const maxAdminScan = 100000

// errNotFirst reports that another sign-up created the root first.
var errNotFirst = errors.New("not the first account")

// signUpRoot creates the bootstrap root as the installation's first account,
// with platform administration and no organisation. The count is repeated
// inside the serialized transaction, so concurrent first sign-ups produce
// one root.
func (s *Service) signUpRoot(ctx context.Context, u User) (User, error) {
	return policyChange(ctx, s, func(tx *Service) (User, error) {
		n, err := tx.St.CountUsers(ctx)
		if err != nil {
			return User{}, err
		}
		if n > 0 {
			return User{}, errNotFirst
		}
		u.BootstrapRoot, u.PlatformAdmin = true, true
		if err := tx.St.CreateUser(ctx, u); err != nil {
			return User{}, err
		}
		if err := tx.St.SaveSetup(ctx, Setup{State: SetupPending, RootUserID: u.ID}); err != nil {
			return User{}, err
		}
		tx.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "anonymous", Action: "setup.root", Target: u.Email, Outcome: Success})
		return u, nil
	})
}

// CompleteSetup hands the installation from root to a standard owner: it
// creates the organisation (or uses an existing one), a distinct standard
// account as its owner, marks setup complete and ends root's sessions, all
// in one transaction. Root does not join the organisation. A retry after
// success gets ErrSetupComplete and creates nothing.
func (s *Service) CompleteSetup(ctx context.Context, p Principal, in SetupInput) (Setup, error) {
	if p.Via != "session" {
		return Setup{}, ErrForbidden
	}
	owner, err := s.newAccount(in.Email, in.Username, in.Name, in.Password)
	if err != nil {
		return Setup{}, err
	}
	if (in.OrgName == "") == (in.OrgID == "") {
		return Setup{}, fmt.Errorf("%w: name a new organisation or choose an existing one", ErrInvalidInput)
	}
	return policyChange(ctx, s, func(tx *Service) (Setup, error) {
		root, err := tx.St.UserByID(ctx, p.User.ID)
		if err != nil || root.DisabledAt != nil {
			return Setup{}, ErrUnauthorized
		}
		setup, err := tx.St.Setup(ctx)
		if err != nil {
			return Setup{}, err
		}
		switch {
		case setup.State == SetupComplete:
			return Setup{}, ErrSetupComplete
		case setup.State != SetupPending || !root.BootstrapRoot || setup.RootUserID != root.ID:
			return Setup{}, ErrForbidden
		}
		if err := tx.ValidatePrincipal(ctx, p); err != nil {
			return Setup{}, err
		}
		if err := tx.St.CreateUser(ctx, owner); err != nil {
			return Setup{}, err
		}
		var org Org
		if in.OrgID != "" {
			if org, err = tx.St.OrgByID(ctx, in.OrgID); err != nil {
				return Setup{}, err
			}
			if err := tx.St.SetMember(ctx, org.ID, owner.ID, Owner); err != nil {
				return Setup{}, err
			}
		} else if org, err = tx.createOrg(ctx, owner.ID, in.OrgName, ""); err != nil {
			return Setup{}, err
		}
		now := tx.now()
		done := Setup{State: SetupComplete, RootUserID: root.ID, OrgID: org.ID, OwnerID: owner.ID, CompletedAt: &now}
		if err := tx.St.SaveSetup(ctx, done); err != nil {
			return Setup{}, err
		}
		// The setup session ends with the handover: business work needs a
		// fresh standard sign-in.
		if err := tx.St.DeleteUserSessions(ctx, root.ID); err != nil {
			return Setup{}, err
		}
		e := actorEvent(p, "setup.complete", org.ID, owner.Email)
		e.Outcome = Success
		tx.audit(ctx, e)
		return done, nil
	})
}

// ReconcileBootstrap classifies an installation that predates bootstrap
// separation, once. The root is recorded only when the setup history agrees:
// the earliest sign-up in the audit log is the earliest account and a
// platform administrator. Anything else records SetupAmbiguous, which fails
// closed for platform administrators until DesignateRoot.
func (s *Service) ReconcileBootstrap(ctx context.Context) (Setup, error) {
	return policyChange(ctx, s, func(tx *Service) (Setup, error) {
		setup, err := tx.St.Setup(ctx)
		if err != nil || setup.State != "" {
			return setup, err
		}
		n, err := tx.St.CountUsers(ctx)
		if err != nil || n == 0 {
			return Setup{}, err // a fresh installation sets up through sign-up
		}
		root, err := tx.historicRoot(ctx)
		if err != nil {
			return Setup{}, err
		}
		if root == "" {
			setup = Setup{State: SetupAmbiguous}
			if err := tx.St.SaveSetup(ctx, setup); err != nil {
				return Setup{}, err
			}
			tx.audit(ctx, Event{Via: "migration", Action: "setup.migrate", Outcome: Failure, Detail: "root not identifiable from setup history; platform administrators limited to governance until designated"})
			return setup, nil
		}
		return tx.classifyRoot(ctx, root, Event{Via: "migration", Action: "setup.migrate", Target: root})
	})
}

// DesignateRoot is the audited operator decision for an ambiguous
// installation (codesynapse-api accounts designate-root). The account must
// be an active platform administrator.
func (s *Service) DesignateRoot(ctx context.Context, email string) (Setup, error) {
	return policyChange(ctx, s, func(tx *Service) (Setup, error) {
		setup, err := tx.St.Setup(ctx)
		if err != nil {
			return Setup{}, err
		}
		if setup.State != SetupAmbiguous {
			return Setup{}, fmt.Errorf("%w: only an installation whose root is ambiguous can designate one", ErrForbidden)
		}
		u, err := tx.St.UserByEmail(ctx, NormalizeEmail(email))
		if err != nil {
			return Setup{}, err
		}
		if !u.PlatformAdmin || u.DisabledAt != nil {
			return Setup{}, fmt.Errorf("%w: the root must be an active platform administrator", ErrInvalidInput)
		}
		return tx.classifyRoot(ctx, u.ID, Event{Via: "operator", Action: "setup.designate_root", Target: u.Email})
	})
}

// classifyRoot marks userID as root and records complete when an active
// standard owner already exists, pending otherwise.
func (s *Service) classifyRoot(ctx context.Context, userID string, e Event) (Setup, error) {
	if err := s.St.MarkBootstrapRoot(ctx, userID); err != nil {
		return Setup{}, err
	}
	setup := Setup{State: SetupPending, RootUserID: userID}
	owner, err := s.standardOwner(ctx)
	if err != nil {
		return Setup{}, err
	}
	if owner != "" {
		now := s.now()
		setup.State, setup.OwnerID, setup.CompletedAt = SetupComplete, owner, &now
	}
	if err := s.St.SaveSetup(ctx, setup); err != nil {
		return Setup{}, err
	}
	e.UserID, e.Outcome, e.Detail = userID, Success, "setup "+setup.State
	s.audit(ctx, e)
	return setup, nil
}

// historicRoot returns the account the setup history identifies, or "".
func (s *Service) historicRoot(ctx context.Context) (string, error) {
	first, err := s.St.ListEvents(ctx, EventFilter{Action: "user.signup", Ascending: true, Limit: 1})
	if err != nil || len(first) == 0 {
		return "", err
	}
	users, err := s.St.ListUsers(ctx, maxAdminScan)
	if err != nil {
		return "", err
	}
	if len(users) >= maxAdminScan {
		return "", nil // too many to establish the earliest
	}
	var earliest *User
	for i := range users {
		if earliest == nil || users[i].CreatedAt.Before(earliest.CreatedAt) {
			earliest = &users[i]
		}
	}
	if earliest.ID != first[0].UserID || !earliest.PlatformAdmin {
		return "", nil
	}
	return earliest.ID, nil
}

// standardOwner returns an active owner of some organisation who is not
// limited to governance, or "".
func (s *Service) standardOwner(ctx context.Context) (string, error) {
	users, err := s.St.ListUsers(ctx, maxAdminScan)
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if u.DisabledAt != nil || businessAllowed(ctx, s.St, u) != nil {
			continue
		}
		ms, err := s.St.UserMemberships(ctx, u.ID)
		if err != nil {
			return "", err
		}
		for _, m := range ms {
			if m.Role == Owner {
				return u.ID, nil
			}
		}
	}
	return "", nil
}

// RecoverOrgOwner restores ownership of an organisation whose owners lost
// access by promoting an existing, active standard member. It is a
// governance action: root reads no organisation content and gains no
// membership.
func (s *Service) RecoverOrgOwner(ctx context.Context, p Principal, orgID, userID string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if err := tx.requirePlatform(ctx, p); err != nil {
			return struct{}{}, err
		}
		if userID == p.User.ID {
			return struct{}{}, ErrForbidden
		}
		u, err := tx.St.UserByID(ctx, userID)
		if err != nil {
			return struct{}{}, err
		}
		if u.DisabledAt != nil || businessAllowed(ctx, tx.St, u) != nil {
			return struct{}{}, fmt.Errorf("%w: the new owner must be an active standard account", ErrInvalidInput)
		}
		if _, err := tx.St.MemberRole(ctx, orgID, userID); err != nil {
			return struct{}{}, fmt.Errorf("%w: the new owner must already be a member", ErrInvalidInput)
		}
		if err := tx.St.SetMember(ctx, orgID, userID, Owner); err != nil {
			return struct{}{}, err
		}
		tx.audit(ctx, actorEvent(p, "org.owner.recover", orgID, u.Email))
		return struct{}{}, nil
	})
	return err
}
