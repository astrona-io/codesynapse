package accounts

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Lifetimes.
const (
	SessionTTL    = 14 * 24 * time.Hour
	keyTouchEvery = time.Minute // last_used_at is written at most this often
)

// Principal is who a request acts as.
type Principal struct {
	User User `json:"user"`
	// OrgID is set for API keys, which act in one organisation only.
	OrgID string `json:"org_id,omitempty"`
	// Via is session, key or service (the legacy single-tenant token).
	Via         string `json:"via"`
	KeyID       string `json:"key_id,omitempty"`
	SessionHash string `json:"-"`
}

// Service applies the account rules on top of a Store.
type Service struct {
	St Store
	// SignupOpen lets anyone create an account after the first one.
	SignupOpen bool
	Now        func() time.Time
	// Sink also receives every audit entry (structured logs).
	Sink Sink
	// Secrets seals organisation Git tokens; nil turns them off.
	Secrets *Sealer

	policy     bool
	policyOrgs map[string]bool
	policyErr  error
}

// Login limits, counted in the store so every API replica shares them.
const (
	loginsPerEmail  = 5
	loginsPerClient = 20
	loginWindow     = time.Minute
)

// NewService returns a service; logins are limited to 5 per minute per
// email and 20 per minute per client address.
func NewService(st Store, signupOpen bool) *Service {
	return &Service{St: st, SignupOpen: signupOpen, Now: time.Now}
}

// allowLogin records a login attempt against both limits. Keys are hashes:
// the table holds neither emails nor addresses.
func (s *Service) allowLogin(ctx context.Context, email, client string, now time.Time) (bool, error) {
	since := now.Add(-loginWindow)
	ok, err := s.St.TakeAttempt(ctx, "email:"+HashToken(email), loginsPerEmail, since, now)
	if err != nil || !ok {
		return false, err
	}
	return s.St.TakeAttempt(ctx, "client:"+HashToken(client), loginsPerClient, since, now)
}

func (s *Service) now() time.Time { return s.Now().UTC().Truncate(time.Microsecond) }

var (
	emailRe = regexp.MustCompile(`^[^\s@]{1,64}@[^\s@]{1,190}\.[^\s@]{2,63}$`)
	slugRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$`)
	nonSlug = regexp.MustCompile(`[^a-z0-9]+`)
)

// NormalizeEmail lower-cases and trims; emails are matched case-insensitively.
func NormalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// usernameRe: 3 to 32 characters, starting with a letter or digit. No "@",
// so a login name is never mistaken for an email address.
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,31}$`)

// NormalizeUsername lower-cases and trims; usernames are case-insensitive.
func NormalizeUsername(u string) string { return strings.ToLower(strings.TrimSpace(u)) }

// CheckUsername validates a username. The empty string is valid and means
// "no username".
func CheckUsername(raw string) (string, error) {
	u := NormalizeUsername(raw)
	if u != "" && !usernameRe.MatchString(u) {
		return "", fmt.Errorf("%w: a username has 3 to 32 letters, digits, dots, dashes or underscores and no @", ErrInvalidInput)
	}
	return u, nil
}

func checkName(name string, what string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if n := utf8.RuneCountInString(name); n < 1 || n > 100 {
		return "", fmt.Errorf("%w: %s needs 1 to 100 characters", ErrInvalidInput, what)
	}
	return name, nil
}

// Slugify derives an organisation slug from a name.
func Slugify(name string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if len(s) < 2 {
		s = "org-" + s
	}
	return s
}

// Status says whether the installation still needs its first account.
func (s *Service) Status(ctx context.Context) (needsSetup bool, err error) {
	n, err := s.St.CountUsers(ctx)
	return n == 0, err
}

// SignUp creates an account. The first account becomes the bootstrap root,
// with platform administration and no organisation (bootstrap.go). Later
// ones need SignupOpen and get a personal organisation; none are accepted
// while root is the only account and setup is pending.
func (s *Service) SignUp(ctx context.Context, email, name, password string) (User, Org, error) {
	return s.SignUpWith(ctx, SignUpInput{Email: email, Name: name, Password: password})
}

// SignUpInput is what a new account supplies. Username is optional.
type SignUpInput struct {
	Email    string
	Username string
	Name     string
	Password string
}

// SignUpWith is SignUp with an optional username.
func (s *Service) SignUpWith(ctx context.Context, in SignUpInput) (User, Org, error) {
	u, err := s.newAccount(in.Email, in.Username, in.Name, in.Password)
	if err != nil {
		return User{}, Org{}, err
	}
	n, err := s.St.CountUsers(ctx)
	if err != nil {
		return User{}, Org{}, err
	}
	if n == 0 {
		root, err := s.signUpRoot(ctx, u)
		if !errors.Is(err, errNotFirst) {
			return root, Org{}, err
		}
		n = 1 // another sign-up became root first
	}
	if !s.SignupOpen {
		return User{}, Org{}, ErrSignupClosed
	}
	if setup, err := s.St.Setup(ctx); err != nil {
		return User{}, Org{}, err
	} else if setup.State == SetupPending && n == 1 {
		return User{}, Org{}, ErrSetupRequired
	}
	// The username first: a taken one is reported whatever the email is, so
	// the answer never depends on whether the email has an account.
	if taken, err := s.usernameTaken(ctx, u.Username); err != nil {
		return User{}, Org{}, err
	} else if taken {
		return User{}, Org{}, ErrUsernameTaken
	}
	if err := s.St.CreateUser(ctx, u); err != nil {
		if !errors.Is(err, ErrExists) {
			return User{}, Org{}, err
		}
		return User{}, Org{}, s.signUpConflict(ctx, u)
	}
	org, err := s.createOrg(ctx, u.ID, u.Name+"'s organisation", "")
	if err != nil {
		return u, Org{}, err
	}
	s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "anonymous", OrgID: org.ID, Action: "user.signup", Target: u.Email})
	return u, org, nil
}

// usernameTaken reports whether another account has the username.
func (s *Service) usernameTaken(ctx context.Context, username string) (bool, error) {
	if username == "" {
		return false, nil
	}
	_, err := s.St.UserByUsername(ctx, username)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// signUpConflict classifies a refused CreateUser. A username taken since the
// check above still reads as a taken username; anything else is the email.
// The real outcome is audited here, because the API answers a taken email
// like a sign-up that worked.
func (s *Service) signUpConflict(ctx context.Context, u User) error {
	if taken, err := s.usernameTaken(ctx, u.Username); err != nil {
		return err
	} else if taken {
		return ErrUsernameTaken
	}
	s.audit(ctx, Event{Via: "anonymous", Action: "user.signup", Target: u.Email, Outcome: Failure, Detail: "email already registered"})
	return ErrEmailTaken
}

// Login checks credentials and returns a new session token. login is an
// email address or a username; a username never contains "@", so the two
// cannot be confused.
func (s *Service) Login(ctx context.Context, login, password, client string) (string, User, error) {
	email := NormalizeEmail(login) // the identifier as audited and rate limited
	now := s.now()
	allowed, err := s.allowLogin(ctx, email, client, now)
	if err != nil {
		return "", User{}, fmt.Errorf("checking login limits: %w", err)
	}
	if !allowed {
		s.audit(ctx, Event{Via: "anonymous", Action: "user.login", Target: email, Outcome: Failure, Detail: "rate limited"})
		return "", User{}, ErrRateLimited
	}
	var u User
	if strings.Contains(email, "@") {
		u, err = s.St.UserByEmail(ctx, email)
	} else {
		u, err = s.St.UserByUsername(ctx, email)
	}
	if errors.Is(err, ErrNotFound) {
		VerifyPassword(password, dummyHash()) // same time for unknown logins
		s.audit(ctx, Event{Via: "anonymous", Action: "user.login", Target: email, Outcome: Failure, Detail: "unknown login"})
		return "", User{}, ErrUnauthorized
	}
	if err != nil {
		return "", User{}, err
	}
	hash, err := s.St.PasswordHash(ctx, u.ID)
	if err != nil {
		return "", User{}, err
	}
	if hash == "" || !VerifyPassword(password, hash) {
		s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "anonymous", Action: "user.login", Target: u.Email, Outcome: Failure, Detail: "wrong password"})
		return "", User{}, ErrUnauthorized
	}
	if u.DisabledAt != nil {
		// Checked after the password, so a suspension is not an oracle for it.
		s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "anonymous", Action: "user.login", Target: u.Email, Outcome: Failure, Detail: "account suspended"})
		return "", User{}, ErrSuspended
	}
	tok, err := s.startSession(ctx, u)
	if err != nil {
		return "", User{}, err
	}
	s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "session", Action: "user.login", Target: u.Email})
	return tok, u, nil
}

// startSession creates a session for u and returns its token.
func (s *Service) startSession(ctx context.Context, u User) (string, error) {
	tok, err := randomToken(32)
	if err != nil {
		return "", err
	}
	now := s.now()
	return tok, s.St.CreateSession(ctx, Session{TokenHash: HashToken(tok), UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(SessionTTL),
		UserAgent: clipText(RequestInfoFrom(ctx).UserAgent, 200)})
}

// Logout ends a session.
func (s *Service) Logout(ctx context.Context, token string) error {
	if sess, err := s.St.SessionByHash(ctx, HashToken(token)); err == nil {
		if u, err := s.St.UserByID(ctx, sess.UserID); err == nil {
			s.audit(ctx, Event{UserID: u.ID, ActorEmail: u.Email, Via: "session", Action: "user.logout", Target: u.Email})
		}
	}
	return s.St.DeleteSession(ctx, HashToken(token))
}

// Authenticate resolves a session token.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthorized
	}
	sess, err := s.St.SessionByHash(ctx, HashToken(token))
	if err != nil || !s.now().Before(sess.ExpiresAt) {
		return Principal{}, ErrUnauthorized
	}
	u, err := s.St.UserByID(ctx, sess.UserID)
	if err != nil || u.DisabledAt != nil {
		return Principal{}, ErrUnauthorized
	}
	return Principal{User: u, Via: "session", SessionHash: sess.TokenHash}, nil
}

// AuthenticateKey resolves an API key.
func (s *Service) AuthenticateKey(ctx context.Context, key string) (Principal, error) {
	if !strings.HasPrefix(key, KeyPrefix) {
		return Principal{}, ErrUnauthorized
	}
	k, err := s.St.APIKeyByHash(ctx, HashToken(key))
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	now := s.now()
	if k.RevokedAt != nil || (k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)) {
		return Principal{}, ErrKeyRevoked
	}
	u, err := s.St.UserByID(ctx, k.UserID)
	if err != nil || u.DisabledAt != nil {
		return Principal{}, ErrUnauthorized
	}
	// Keys exist for organisation work, which root may not do.
	if err := businessAllowed(ctx, s.St, u); err != nil {
		return Principal{}, err
	}
	// A key outlives a membership only on paper: removed members lose access.
	if _, err := s.St.MemberRole(ctx, k.OrgID, u.ID); err != nil {
		return Principal{}, ErrForbidden
	}
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) >= keyTouchEvery {
		_ = s.St.TouchAPIKey(ctx, k.ID, now) //nolint:errcheck // usage time is informational
	}
	return Principal{User: u, OrgID: k.OrgID, Via: "key", KeyID: k.ID}, nil
}

// RoleIn returns the explicitly enrolled user's role; API keys
// only work in their own organisation.
func (s *Service) RoleIn(ctx context.Context, p Principal, orgID string) (Role, error) {
	if p.OrgID != "" && p.OrgID != orgID {
		return "", ErrForbidden
	}
	if err := s.ValidatePrincipal(ctx, p); err != nil {
		return "", ErrForbidden
	}
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil {
		return "", ErrForbidden
	}
	if err := businessAllowed(ctx, s.St, u); err != nil {
		return "", err
	}
	r, err := s.St.MemberRole(ctx, orgID, p.User.ID)
	if errors.Is(err, ErrNotFound) {
		return "", ErrForbidden
	}
	if err != nil {
		return "", err
	}
	if r == Owner {
		return r, nil
	} // An explicit owner can repair broken team policy.
	// Roles granted to the user's teams count too (ReBAC, authz.go).
	tr, err := s.teamOrgRole(ctx, orgID, p.User.ID)
	return Max(r, tr), err
}

// teamOrgRole is the strongest organisation role the user holds through a
// team ("" without one).
func (s *Service) teamOrgRole(ctx context.Context, orgID, userID string) (Role, error) {
	mine, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, SubjectType: TypeUser, SubjectID: userID})
	if err != nil || len(mine) == 0 {
		return "", err
	}
	ids := make([]string, len(mine))
	for i, t := range mine {
		ids[i] = t.ObjectID
	}
	grants, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeOrganisation, ObjectID: orgID, SubjectType: TypeTeam, SubjectIDs: ids, SubjectRelation: RelMember})
	var best Role
	for _, g := range grants {
		best = Max(best, Role(g.Relation))
	}
	return best, err
}

// Require returns ErrForbidden unless p has at least the role least in org.
func (s *Service) Require(ctx context.Context, p Principal, orgID string, least Role) error {
	r, err := s.RoleIn(ctx, p, orgID)
	if err != nil {
		return err
	}
	if !r.AtLeast(least) {
		return ErrForbidden
	}
	return nil
}

// CreateOrg creates an organisation owned by p.
func (s *Service) CreateOrg(ctx context.Context, p Principal, name, slug string) (Org, error) {
	if p.Via == "key" {
		return Org{}, ErrForbidden // keys act inside one organisation
	}
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil {
		return Org{}, ErrForbidden
	}
	if err := businessAllowed(ctx, s.St, u); err != nil {
		return Org{}, err
	}
	o, err := s.createOrg(ctx, p.User.ID, name, slug)
	if err == nil {
		s.audit(ctx, actorEvent(p, "org.create", o.ID, o.Slug))
	}
	return o, err
}

func (s *Service) createOrg(ctx context.Context, owner, name, slug string) (Org, error) {
	name, err := checkName(name, "an organisation name")
	if err != nil {
		return Org{}, err
	}
	explicit := slug != ""
	if !explicit {
		slug = Slugify(name)
	}
	if !slugRe.MatchString(slug) {
		return Org{}, fmt.Errorf("%w: slugs are 2 to 40 lower-case letters, digits and dashes", ErrInvalidInput)
	}
	id, err := newID()
	if err != nil {
		return Org{}, err
	}
	base := slug
	for i := 2; ; i++ {
		o := Org{ID: id, Slug: slug, Name: name, CreatedAt: s.now(), DefaultRepoAccess: DefaultRepoNone, PolicyRevision: 1}
		err := s.St.CreateOrg(ctx, o, owner)
		if err == nil {
			return o, nil
		}
		if !errors.Is(err, ErrExists) || explicit || i > 50 {
			return Org{}, err
		}
		slug = fmt.Sprintf("%s-%d", strings.TrimRight(base[:min(len(base), 36)], "-"), i)
	}
}

// AddMember gives an existing user a role in org.
func (s *Service) AddMember(ctx context.Context, p Principal, orgID, email string, role Role) (Membership, error) {
	return policyChange(ctx, s, func(tx *Service) (Membership, error) { return tx.addMemberPolicy(ctx, p, orgID, email, role) })
}

func (s *Service) addMemberPolicy(ctx context.Context, p Principal, orgID, email string, role Role) (Membership, error) {
	if p.Via == "key" {
		return Membership{}, ErrForbidden
	}
	if !role.Valid() {
		return Membership{}, fmt.Errorf("%w: unknown role %q", ErrInvalidInput, role)
	}
	mine, err := s.RoleIn(ctx, p, orgID)
	if err != nil {
		return Membership{}, err
	}
	if !mine.AtLeast(Admin) || (role == Owner && mine != Owner) {
		return Membership{}, ErrForbidden
	}
	u, err := s.St.UserByEmail(ctx, NormalizeEmail(email))
	if err != nil {
		return Membership{}, err
	}
	if _, err := s.St.MemberRole(ctx, orgID, u.ID); errors.Is(err, ErrNotFound) && mine != Owner {
		return Membership{}, ErrForbidden
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return Membership{}, err
	}
	if err := s.guardLastOwner(ctx, orgID, u.ID, role); err != nil {
		return Membership{}, err
	}
	if err := s.St.SetMember(ctx, orgID, u.ID, role); err != nil {
		return Membership{}, err
	}
	e := actorEvent(p, "member.set", orgID, u.Email)
	e.Detail = "role " + string(role)
	s.audit(ctx, e)
	o, err := s.St.OrgByID(ctx, orgID)
	return Membership{Org: o, User: u, Role: role, AddedAt: s.now()}, err
}

// RemoveMember removes a user from org; members may remove themselves.
func (s *Service) RemoveMember(ctx context.Context, p Principal, orgID, userID string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) { return struct{}{}, tx.removeMemberPolicy(ctx, p, orgID, userID) })
	return err
}

func (s *Service) removeMemberPolicy(ctx context.Context, p Principal, orgID, userID string) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	if userID != p.User.ID {
		if err := s.Require(ctx, p, orgID, Admin); err != nil {
			return err
		}
	} else if _, err := s.RoleIn(ctx, p, orgID); err != nil {
		return err
	}
	if err := s.guardLastOwner(ctx, orgID, userID, ""); err != nil {
		return err
	}
	admins, err := s.adminRepositories(ctx, orgID)
	if err != nil {
		return err
	}
	if err := s.St.RemoveMember(ctx, orgID, userID); err != nil {
		return err
	}
	if err := s.dropUserTuples(ctx, orgID, userID); err != nil {
		return err
	}
	if err := s.preserveAdmins(ctx, orgID, admins); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "member.remove", orgID, userID))
	return nil
}

// guardLastOwner refuses to leave org without an owner who can act: an
// owner limited to governance (the bootstrap root) does not count.
func (s *Service) guardLastOwner(ctx context.Context, orgID, userID string, newRole Role) error {
	if newRole == Owner {
		return nil
	}
	members, err := s.St.OrgMembers(ctx, orgID)
	if err != nil {
		return err
	}
	owners, target := 0, false
	for _, m := range members {
		if m.Role != Owner {
			continue
		}
		if err := businessAllowed(ctx, s.St, m.User); errors.Is(err, ErrRootAccount) {
			continue
		} else if err != nil {
			return err
		}
		owners++
		if m.User.ID == userID {
			target = true
		}
	}
	if target && owners == 1 {
		return ErrLastOwner
	}
	return nil
}

// CreateKey makes a personal API key for org and returns the secret once.
func (s *Service) CreateKey(ctx context.Context, p Principal, orgID, name string, ttl time.Duration) (string, APIKey, error) {
	if p.Via == "key" {
		return "", APIKey{}, ErrForbidden // keys do not mint keys
	}
	if _, err := s.RoleIn(ctx, p, orgID); err != nil {
		return "", APIKey{}, err
	}
	name, err := checkName(name, "a key name")
	if err != nil {
		return "", APIKey{}, err
	}
	key, prefix, err := newAPIKey()
	if err != nil {
		return "", APIKey{}, err
	}
	id, err := newID()
	if err != nil {
		return "", APIKey{}, err
	}
	k := APIKey{ID: id, UserID: p.User.ID, OrgID: orgID, Name: name, Prefix: prefix, Hash: HashToken(key), CreatedAt: s.now()}
	if ttl > 0 {
		exp := k.CreatedAt.Add(ttl)
		k.ExpiresAt = &exp
	}
	if err := s.St.CreateAPIKey(ctx, k); err != nil {
		return "", APIKey{}, err
	}
	s.audit(ctx, actorEvent(p, "key.create", orgID, prefix))
	return key, k, nil
}

// RevokeKey revokes one of p's keys.
func (s *Service) RevokeKey(ctx context.Context, p Principal, keyID string) error {
	if err := s.St.RevokeAPIKey(ctx, p.User.ID, keyID, s.now()); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "key.revoke", "", keyID))
	return nil
}

// ChangePassword sets a new password and ends every session of the user.
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next string) error {
	if p.Via == "key" {
		return ErrForbidden
	}
	u, err := s.St.UserByID(ctx, p.User.ID)
	if err != nil {
		return err
	}
	old, err := s.St.PasswordHash(ctx, u.ID)
	if err != nil {
		return err
	}
	if !VerifyPassword(current, old) {
		return ErrUnauthorized
	}
	if err := CheckPassword(next); err != nil {
		return err
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	if err := s.St.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	s.audit(ctx, actorEvent(p, "user.password", "", u.Email))
	return s.St.DeleteUserSessions(ctx, u.ID)
}

// audit records a service event. A storage failure is logged by the sink's
// owner through Record's error path; the action itself stands.
func (s *Service) audit(ctx context.Context, e Event) {
	if s.policy && e.OrgID != "" {
		s.policyOrgs[e.OrgID] = true
	}
	_, err := s.Record(ctx, e)
	if s.policy && err != nil {
		s.policyErr = err
	}
}
