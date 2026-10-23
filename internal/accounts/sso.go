package accounts

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Single sign-on (docs/SSO_PLAN.md). Each organisation may connect one
// OpenID Connect provider. Signing in through it creates the account on
// first use, links it to the provider's subject, and lets the provider's
// group claims decide the organisation role and teams. The network side
// (discovery, token exchange, ID-token verification) lives in package sso;
// this file holds the rules and the records.

// SSO limits.
const (
	SSOStateTTL      = 10 * time.Minute
	MaxGroupMappings = 100
	MaxClaimGroups   = 500
	maxGroupName     = 256
	ssoSecretPurpose = "oidc-client-secret" //nolint:gosec // a sealing purpose label, not a credential
)

// SSO errors.
var (
	// ErrSSOLinkRequired: an account with the IdP's email exists but may not
	// be linked automatically.
	ErrSSOLinkRequired = errors.New("an account with this email already exists: sign in with your password and link single sign-on from your profile")
	// ErrSSONoAccess: the IdP grants no role in its organisation.
	ErrSSONoAccess = errors.New("your identity provider does not grant access to this organisation; ask its administrator")
	// ErrSSOState: unknown, used, expired or foreign login state.
	ErrSSOState = errors.New("the sign-in link expired or was already used; start again")
)

// SSO modes.
const (
	SSOLogin = "login"
	SSOLink  = "link"
)

// IdentityProvider is an organisation's OpenID Connect provider. The client
// secret is sealed; only a preview leaves the store.
type IdentityProvider struct {
	OrgID         string `json:"org_id"`
	Name          string `json:"name"`
	Issuer        string `json:"issuer"`
	ClientID      string `json:"client_id"`
	SecretPreview string `json:"secret_preview"`
	KeyID         string `json:"-"`
	SealedSecret  []byte `json:"-"`
	IdentityProviderSettings
	Enabled   bool      `json:"enabled"`
	CreatedBy string    `json:"created_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IdentityProviderSettings decide what a sign-in grants.
type IdentityProviderSettings struct {
	// Scopes are requested besides openid, email and profile.
	Scopes []string `json:"scopes,omitempty"`
	// GroupsClaim names the ID-token claim listing the user's groups.
	GroupsClaim string `json:"groups_claim"`
	// DefaultRole is granted when no group maps to a role; empty grants no
	// membership, so only mapped groups get in.
	DefaultRole Role `json:"default_role,omitempty"`
	// RoleGroups maps a group to an organisation role; the strongest wins.
	RoleGroups map[string]Role `json:"role_groups,omitempty"`
	// TeamGroups maps a group to a team of the organisation (team ID).
	TeamGroups map[string]string `json:"team_groups,omitempty"`
}

// IdentityProviderInput configures a provider. An empty ClientSecret keeps
// the stored one on update.
type IdentityProviderInput struct {
	Name         string `json:"name"`
	Issuer       string `json:"issuer"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Enabled      bool   `json:"enabled"`
	IdentityProviderSettings
}

// ExternalIdentity links a provider subject to an account.
type ExternalIdentity struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	Issuer      string     `json:"issuer"`
	Subject     string     `json:"-"`
	UserID      string     `json:"user_id"`
	Email       string     `json:"email"`
	LinkedAt    time.Time  `json:"linked_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// SSOState is a pending sign-in: single use, bound to the browser that
// started it, valid for SSOStateTTL.
type SSOState struct {
	StateHash  string
	BinderHash string
	OrgID      string
	Mode       string
	UserID     string // the signed-in user for SSOLink
	Nonce      string
	Verifier   string // PKCE code verifier
	Next       string // dashboard path to return to
	ExpiresAt  time.Time
}

// SSOStart is what the API needs to send the browser to the provider.
type SSOStart struct {
	State    string
	Binder   string // set as an HTTP-only cookie, checked on return
	Nonce    string
	Verifier string
	Provider IdentityProvider
	Secret   string
}

// SSOClaims are the verified ID-token claims a sign-in uses.
type SSOClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
}

// SetIdentityProvider creates or updates an organisation's provider. Owners
// and admins only, in a signed-in session (never an API key). The caller has
// already checked that the issuer answers OpenID discovery.
func (s *Service) SetIdentityProvider(ctx context.Context, p Principal, orgID string, in IdentityProviderInput) (IdentityProvider, error) {
	if p.Via != "session" {
		return IdentityProvider{}, ErrForbidden
	}
	if s.Secrets == nil {
		return IdentityProvider{}, ErrNoSecretKey
	}
	issuer, err := CheckIssuer(in.Issuer)
	if err != nil {
		return IdentityProvider{}, err
	}
	name, err := checkName(in.Name, "a provider name")
	if err != nil {
		return IdentityProvider{}, err
	}
	in.ClientID = strings.TrimSpace(in.ClientID)
	if in.ClientID == "" || len(in.ClientID) > 256 {
		return IdentityProvider{}, fmt.Errorf("%w: a client ID is needed", ErrInvalidInput)
	}
	if len(in.ClientSecret) > 4096 {
		return IdentityProvider{}, fmt.Errorf("%w: the client secret is too long", ErrInvalidInput)
	}
	settings, err := checkSettings(in.IdentityProviderSettings)
	if err != nil {
		return IdentityProvider{}, err
	}
	return policyChange(ctx, s, func(tx *Service) (IdentityProvider, error) {
		if err := tx.Require(ctx, p, orgID, Admin); err != nil {
			return IdentityProvider{}, err
		}
		for _, teamID := range settings.TeamGroups {
			t, err := tx.St.TeamByID(ctx, teamID)
			if err != nil || t.OrgID != orgID {
				return IdentityProvider{}, fmt.Errorf("%w: team %q is not a team of this organisation", ErrInvalidInput, teamID)
			}
		}
		idp := IdentityProvider{OrgID: orgID, Name: name, Issuer: issuer, ClientID: in.ClientID, IdentityProviderSettings: settings,
			Enabled: in.Enabled, CreatedBy: p.User.ID, UpdatedAt: tx.now()}
		old, err := tx.St.IdentityProvider(ctx, orgID)
		switch {
		case in.ClientSecret != "":
			if idp.SealedSecret, err = tx.Secrets.SealFor(ssoSecretPurpose, in.ClientSecret, orgID, issuer); err != nil {
				return IdentityProvider{}, err
			}
			idp.KeyID, idp.SecretPreview = tx.Secrets.KeyID, Preview(in.ClientSecret)
		case errors.Is(err, ErrNotFound):
			return IdentityProvider{}, fmt.Errorf("%w: a client secret is needed", ErrInvalidInput)
		case err != nil:
			return IdentityProvider{}, err
		case old.Issuer != issuer:
			// The secret is bound to the issuer: a new issuer needs it again.
			return IdentityProvider{}, fmt.Errorf("%w: enter the client secret again for the new issuer", ErrInvalidInput)
		default:
			idp.SealedSecret, idp.KeyID, idp.SecretPreview, idp.CreatedBy = old.SealedSecret, old.KeyID, old.SecretPreview, old.CreatedBy
		}
		if err := tx.St.PutIdentityProvider(ctx, idp); err != nil {
			return IdentityProvider{}, err
		}
		e := actorEvent(p, "org.sso.set", orgID, issuer)
		e.Detail = fmt.Sprintf("enabled=%v default_role=%q role_groups=%d team_groups=%d", idp.Enabled, idp.DefaultRole, len(idp.RoleGroups), len(idp.TeamGroups))
		tx.audit(ctx, e)
		return idp, nil
	})
}

// IdentityProviderOf returns an organisation's provider for its admins.
func (s *Service) IdentityProviderOf(ctx context.Context, p Principal, orgID string) (IdentityProvider, error) {
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return IdentityProvider{}, err
	}
	return s.St.IdentityProvider(ctx, orgID)
}

// RemoveIdentityProvider disconnects an organisation's provider. Linked
// identities stay (they stop working) so reconnecting keeps accounts.
func (s *Service) RemoveIdentityProvider(ctx context.Context, p Principal, orgID string) error {
	if p.Via != "session" {
		return ErrForbidden
	}
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if err := tx.Require(ctx, p, orgID, Admin); err != nil {
			return struct{}{}, err
		}
		if err := tx.St.DeleteIdentityProvider(ctx, orgID); err != nil {
			return struct{}{}, err
		}
		tx.audit(ctx, actorEvent(p, "org.sso.remove", orgID, orgID))
		return struct{}{}, nil
	})
	return err
}

// BeginSSO records a pending sign-in for an organisation's enabled provider
// and returns what the browser needs. For SSOLink, p must be the signed-in
// user; for SSOLogin it is ignored.
func (s *Service) BeginSSO(ctx context.Context, orgSlug, mode string, p Principal, next string) (SSOStart, error) {
	if mode != SSOLogin && mode != SSOLink {
		return SSOStart{}, ErrInvalidInput
	}
	if s.Secrets == nil {
		return SSOStart{}, ErrNoSecretKey
	}
	org, err := s.St.OrgBySlug(ctx, orgSlug)
	if err != nil {
		return SSOStart{}, ErrNotFound
	}
	idp, err := s.St.IdentityProvider(ctx, org.ID)
	if err != nil || !idp.Enabled {
		return SSOStart{}, fmt.Errorf("%w: this organisation has no single sign-on", ErrNotFound)
	}
	userID := ""
	if mode == SSOLink {
		if p.Via != "session" {
			return SSOStart{}, ErrForbidden
		}
		u, err := s.St.UserByID(ctx, p.User.ID)
		if err != nil || u.DisabledAt != nil {
			return SSOStart{}, ErrUnauthorized
		}
		if err := businessAllowed(ctx, s.St, u); err != nil {
			return SSOStart{}, err
		}
		userID = u.ID
	}
	secret, err := s.Secrets.OpenFor(ssoSecretPurpose, idp.SealedSecret, idp.KeyID, org.ID, idp.Issuer)
	if err != nil {
		return SSOStart{}, err
	}
	start := SSOStart{Provider: idp, Secret: secret}
	for _, v := range []*string{&start.State, &start.Binder, &start.Nonce, &start.Verifier} {
		if *v, err = randomToken(32); err != nil {
			return SSOStart{}, err
		}
	}
	now := s.now()
	_ = s.St.PruneSSOStates(ctx, now) //nolint:errcheck // housekeeping; expired states are refused anyway
	st := SSOState{StateHash: HashToken(start.State), BinderHash: HashToken(start.Binder), OrgID: org.ID, Mode: mode, UserID: userID,
		Nonce: start.Nonce, Verifier: start.Verifier, Next: safeNext(next), ExpiresAt: now.Add(SSOStateTTL)}
	if err := s.St.PutSSOState(ctx, st); err != nil {
		return SSOStart{}, err
	}
	return start, nil
}

// TakeSSO consumes a pending sign-in. It fails unless the state exists, has
// not expired and was started by the browser presenting binder.
func (s *Service) TakeSSO(ctx context.Context, state, binder string) (SSOState, IdentityProvider, string, error) {
	st, err := s.St.TakeSSOState(ctx, HashToken(state))
	if err != nil || binder == "" || subtle.ConstantTimeCompare([]byte(st.BinderHash), []byte(HashToken(binder))) != 1 || !s.now().Before(st.ExpiresAt) {
		s.audit(ctx, Event{Via: "anonymous", Action: "sso.login", Outcome: Failure, Detail: "invalid state"})
		return SSOState{}, IdentityProvider{}, "", ErrSSOState
	}
	idp, err := s.St.IdentityProvider(ctx, st.OrgID)
	if err != nil || !idp.Enabled {
		return SSOState{}, IdentityProvider{}, "", ErrSSOState
	}
	secret, err := s.Secrets.OpenFor(ssoSecretPurpose, idp.SealedSecret, idp.KeyID, st.OrgID, idp.Issuer)
	if err != nil {
		return SSOState{}, IdentityProvider{}, "", err
	}
	return st, idp, secret, nil
}

// CompleteSSOLogin signs in with verified claims: it finds or creates the
// account, links the identity, applies the provider's membership decision and
// starts a session. Root accounts never sign in this way.
func (s *Service) CompleteSSOLogin(ctx context.Context, st SSOState, idp IdentityProvider, c SSOClaims) (string, User, error) {
	if st.Mode != SSOLogin {
		return "", User{}, ErrSSOState
	}
	if err := checkClaims(&c); err != nil {
		return "", User{}, err
	}
	fail := func(u User, detail string, err error) (string, User, error) {
		s.audit(ctx, Event{UserID: u.ID, ActorEmail: c.Email, Via: "sso", OrgID: idp.OrgID, Action: "sso.login", Target: c.Email, Outcome: Failure, Detail: detail})
		return "", User{}, err
	}
	role, teams := idp.grants(c.Groups)
	type outcome struct {
		u       User
		allowed bool
	}
	// Commit the provider's decision even when it refuses entry: a removed
	// membership must stay removed.
	out, err := policyChange(ctx, s, func(tx *Service) (outcome, error) {
		u, err := tx.ssoAccount(ctx, idp, c, role)
		if err != nil {
			return outcome{u: u}, err
		}
		allowed, err := tx.syncMembership(ctx, idp, u, role, teams)
		return outcome{u: u, allowed: allowed}, err
	})
	u := out.u
	if err != nil {
		return fail(u, err.Error(), err)
	}
	if !out.allowed {
		return fail(u, "no role granted", ErrSSONoAccess)
	}
	tok, err := s.startSession(ctx, u)
	if err != nil {
		return "", User{}, err
	}
	s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "sso", OrgID: idp.OrgID, Action: "sso.login", Target: u.Email, Outcome: Success,
		Detail: "role " + string(role)})
	return tok, u, nil
}

// ssoAccount finds the account for verified claims, linking or creating it
// under the guarded rules.
func (s *Service) ssoAccount(ctx context.Context, idp IdentityProvider, c SSOClaims, role Role) (User, error) {
	now := s.now()
	id, err := s.St.IdentityBySubject(ctx, idp.Issuer, c.Subject)
	switch {
	case err == nil:
		u, err := s.St.UserByID(ctx, id.UserID)
		if err != nil {
			return User{}, err
		}
		if err := ssoAllowed(ctx, s.St, u); err != nil {
			return u, err
		}
		id.Email, id.LastLoginAt = c.Email, &now
		return u, s.St.PutIdentity(ctx, id)
	case !errors.Is(err, ErrNotFound):
		return User{}, err
	}
	if c.Email == "" || !c.EmailVerified {
		return User{}, fmt.Errorf("%w: the identity provider must send a verified email address", ErrInvalidInput)
	}
	u, err := s.St.UserByEmail(ctx, c.Email)
	switch {
	case err == nil:
		// Link only an account already in this provider's organisation:
		// another organisation's IdP cannot take over someone's account.
		if err := ssoAllowed(ctx, s.St, u); err != nil {
			return u, err
		}
		if _, err := s.St.MemberRole(ctx, idp.OrgID, u.ID); err != nil {
			return u, ErrSSOLinkRequired
		}
	case errors.Is(err, ErrNotFound):
		if role == "" {
			return User{}, ErrSSONoAccess // no account for someone who may not enter
		}
		if u, err = s.jitAccount(c); err != nil {
			return User{}, err
		}
		if err := s.St.CreateUser(ctx, u); err != nil {
			return User{}, err
		}
		s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "sso", OrgID: idp.OrgID, Action: "user.signup", Target: u.Email, Outcome: Success, Detail: "single sign-on"})
	default:
		return User{}, err
	}
	if err := s.link(ctx, idp, c, u, now); err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *Service) jitAccount(c SSOClaims) (User, error) {
	id, err := newID()
	if err != nil {
		return User{}, err
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name, _, _ = strings.Cut(c.Email, "@")
	}
	if name, err = checkName(name, "a name"); err != nil {
		return User{}, err
	}
	// No password: the account signs in through its provider only.
	return User{ID: id, Email: c.Email, Name: name, CreatedAt: s.now()}, nil
}

func (s *Service) link(ctx context.Context, idp IdentityProvider, c SSOClaims, u User, now time.Time) error {
	id, err := newID()
	if err != nil {
		return err
	}
	if err := s.St.PutIdentity(ctx, ExternalIdentity{ID: id, OrgID: idp.OrgID, Issuer: idp.Issuer, Subject: c.Subject, UserID: u.ID, Email: c.Email, LinkedAt: now, LastLoginAt: &now}); err != nil {
		return err
	}
	s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "sso", OrgID: idp.OrgID, Action: "sso.link", Target: idp.Issuer, Outcome: Success})
	return nil
}

// ssoAllowed refuses suspended and governance-only accounts.
func ssoAllowed(ctx context.Context, st Store, u User) error {
	if u.DisabledAt != nil {
		return ErrSuspended
	}
	return businessAllowed(ctx, st, u)
}

// syncMembership applies the provider's decision to the membership it
// manages and reports whether the sign-in may proceed. A membership an owner
// added by hand is never changed; one the provider created follows its role
// and mapped teams on every sign-in and ends when the provider stops
// granting a role. The last owner is never removed or demoted this way: the
// membership stays (root can recover the organisation) and the sign-in is
// refused.
func (s *Service) syncMembership(ctx context.Context, idp IdentityProvider, u User, role Role, teams []string) (bool, error) {
	_, err := s.St.MemberRole(ctx, idp.OrgID, u.ID)
	member := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	managed := false
	if member {
		if managed, err = s.St.ManagedByIDP(ctx, idp.OrgID, u.ID); err != nil {
			return false, err
		}
	}
	p := Principal{User: u, Via: "sso"}
	event := func(action, detail string, outcome string) {
		e := actorEvent(p, action, idp.OrgID, u.Email)
		e.Detail, e.Outcome = detail, outcome
		s.audit(ctx, e)
	}
	switch {
	case member && !managed:
		return true, nil // owners decide hand-added memberships
	case !member && role == "":
		return false, nil
	case !member:
		if err := s.St.SetMember(ctx, idp.OrgID, u.ID, role); err != nil {
			return false, err
		}
		if err := s.St.SetManagedByIDP(ctx, idp.OrgID, u.ID, true); err != nil {
			return false, err
		}
		event("sso.member.add", string(role), Success)
	case role == "":
		if err := s.guardLastOwner(ctx, idp.OrgID, u.ID, ""); errors.Is(err, ErrLastOwner) {
			event("sso.member.remove", "kept: last owner", Failure)
			return false, nil
		} else if err != nil {
			return false, err
		}
		if err := s.St.RemoveMember(ctx, idp.OrgID, u.ID); err != nil {
			return false, err
		}
		event("sso.member.remove", "", Success)
		return false, nil
	default:
		current, err := s.St.MemberRole(ctx, idp.OrgID, u.ID)
		if err != nil {
			return false, err
		}
		if current != role {
			if err := s.guardLastOwner(ctx, idp.OrgID, u.ID, role); errors.Is(err, ErrLastOwner) {
				event("sso.member.role", "kept: last owner", Failure)
				return false, nil
			} else if err != nil {
				return false, err
			}
			if err := s.St.SetMember(ctx, idp.OrgID, u.ID, role); err != nil {
				return false, err
			}
			event("sso.member.role", string(current)+" -> "+string(role), Success)
		}
	}
	return true, s.syncTeams(ctx, idp, u, teams)
}

// syncTeams adds the user to granted teams and removes them from mapped
// teams the provider no longer grants. Unmapped teams are left alone.
func (s *Service) syncTeams(ctx context.Context, idp IdentityProvider, u User, granted []string) error {
	mapped := map[string]bool{}
	for _, teamID := range idp.TeamGroups {
		mapped[teamID] = true
	}
	for teamID := range mapped {
		want := slices.Contains(granted, teamID)
		have, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: teamID, SubjectType: TypeUser, SubjectID: u.ID})
		if err != nil {
			return err
		}
		switch {
		case want && len(have) == 0:
			if err := s.St.WriteTuple(ctx, Tuple{ObjectType: TypeTeam, ObjectID: teamID, Relation: RelMember, SubjectType: TypeUser, SubjectID: u.ID,
				CreatedBy: u.ID, CreatedAt: s.now()}); err != nil {
				return err
			}
			s.audit(ctx, actorEvent(Principal{User: u, Via: "sso"}, "sso.team.add", idp.OrgID, teamID))
		case !want && len(have) > 0:
			if _, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: teamID, SubjectType: TypeUser, SubjectID: u.ID}); err != nil {
				return err
			}
			s.audit(ctx, actorEvent(Principal{User: u, Via: "sso"}, "sso.team.remove", idp.OrgID, teamID))
		}
	}
	return nil
}

// CompleteSSOLink links verified claims to the signed-in user who started
// the flow. The subject must not belong to anyone else.
func (s *Service) CompleteSSOLink(ctx context.Context, st SSOState, idp IdentityProvider, c SSOClaims) error {
	if st.Mode != SSOLink || st.UserID == "" {
		return ErrSSOState
	}
	if err := checkClaims(&c); err != nil {
		return err
	}
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		u, err := tx.St.UserByID(ctx, st.UserID)
		if err != nil {
			return struct{}{}, err
		}
		if err := ssoAllowed(ctx, tx.St, u); err != nil {
			return struct{}{}, err
		}
		existing, err := tx.St.IdentityBySubject(ctx, idp.Issuer, c.Subject)
		switch {
		case err == nil && existing.UserID == u.ID:
			return struct{}{}, nil
		case err == nil:
			return struct{}{}, fmt.Errorf("%w: this identity is linked to another account", ErrExists)
		case !errors.Is(err, ErrNotFound):
			return struct{}{}, err
		}
		return struct{}{}, tx.link(ctx, idp, c, u, tx.now())
	})
	return err
}

// Identities lists the signed-in user's linked identities.
func (s *Service) Identities(ctx context.Context, p Principal) ([]ExternalIdentity, error) {
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		return nil, err
	}
	return s.St.UserIdentities(ctx, p.User.ID)
}

// UnlinkIdentity removes one of the signed-in user's identities. An account
// without a password keeps at least one identity, or it could not sign in.
func (s *Service) UnlinkIdentity(ctx context.Context, p Principal, id string) error {
	if p.Via != "session" {
		return ErrForbidden
	}
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		u, err := tx.St.UserByID(ctx, p.User.ID)
		if err != nil {
			return struct{}{}, err
		}
		ids, err := tx.St.UserIdentities(ctx, u.ID)
		if err != nil {
			return struct{}{}, err
		}
		i := slices.IndexFunc(ids, func(e ExternalIdentity) bool { return e.ID == id })
		if i < 0 {
			return struct{}{}, ErrNotFound
		}
		if u.PasswordHash == "" && len(ids) == 1 {
			return struct{}{}, fmt.Errorf("%w: this is the only way this account signs in", ErrForbidden)
		}
		if err := tx.St.DeleteIdentity(ctx, u.ID, id); err != nil {
			return struct{}{}, err
		}
		tx.audit(ctx, actorEvent(p, "sso.unlink", ids[i].OrgID, ids[i].Issuer))
		return struct{}{}, nil
	})
	return err
}

// grants maps the user's groups to a role and teams.
func (idp IdentityProvider) grants(groups []string) (Role, []string) {
	role := idp.DefaultRole
	var mappedRole Role
	var teams []string
	for _, g := range groups {
		if r, ok := idp.RoleGroups[g]; ok {
			mappedRole = Max(mappedRole, r)
		}
		if t, ok := idp.TeamGroups[g]; ok && !slices.Contains(teams, t) {
			teams = append(teams, t)
		}
	}
	if mappedRole != "" {
		role = mappedRole
	}
	return role, teams
}

// CheckIssuer accepts an https issuer URL without credentials, query or
// fragment. Address checks (no internal hosts) happen where it is fetched.
func CheckIssuer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(raw) > 512 {
		return "", fmt.Errorf("%w: the issuer must be an https URL without credentials, query or fragment", ErrInvalidInput)
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func checkSettings(s IdentityProviderSettings) (IdentityProviderSettings, error) {
	if s.GroupsClaim == "" {
		s.GroupsClaim = "groups"
	}
	if len(s.GroupsClaim) > 64 || strings.ContainsAny(s.GroupsClaim, " \t\n") {
		return s, fmt.Errorf("%w: invalid groups claim name", ErrInvalidInput)
	}
	if s.DefaultRole != "" && (!s.DefaultRole.Valid() || s.DefaultRole == Owner) {
		return s, fmt.Errorf("%w: the default role must be guest, viewer, member or admin", ErrInvalidInput)
	}
	if len(s.RoleGroups)+len(s.TeamGroups) > MaxGroupMappings || len(s.Scopes) > 10 {
		return s, fmt.Errorf("%w: at most %d group mappings and 10 scopes", ErrInvalidInput, MaxGroupMappings)
	}
	for g, r := range s.RoleGroups {
		if g == "" || len(g) > maxGroupName || !r.Valid() {
			return s, fmt.Errorf("%w: invalid role mapping for group %q", ErrInvalidInput, g)
		}
	}
	for g, t := range s.TeamGroups {
		if g == "" || len(g) > maxGroupName || t == "" {
			return s, fmt.Errorf("%w: invalid team mapping for group %q", ErrInvalidInput, g)
		}
	}
	for _, sc := range s.Scopes {
		if sc == "" || len(sc) > 64 || strings.ContainsAny(sc, " \t\n") {
			return s, fmt.Errorf("%w: invalid scope %q", ErrInvalidInput, sc)
		}
	}
	return s, nil
}

func checkClaims(c *SSOClaims) error {
	if c.Subject == "" || len(c.Subject) > 255 {
		return fmt.Errorf("%w: the ID token has no usable subject", ErrInvalidInput)
	}
	c.Email = NormalizeEmail(c.Email)
	if c.Email != "" && !emailRe.MatchString(c.Email) {
		c.Email, c.EmailVerified = "", false
	}
	if len(c.Groups) > MaxClaimGroups {
		c.Groups = c.Groups[:MaxClaimGroups]
	}
	return nil
}

// safeNext keeps a return path inside the dashboard.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") || len(next) > 512 {
		return "/overview"
	}
	return next
}
