// Package accounts holds users, organisations (tenants), memberships,
// sessions and API keys, and the rules around them: password hashing,
// token generation, roles and who may do what. Storage is behind Store
// (PostgreSQL in production, memory in tests); see docs/SAAS.md.
package accounts

import (
	"context"
	"errors"
	"time"
)

// Role is a user's role in an organisation, weakest first.
type Role string

// Roles.
const (
	Viewer Role = "viewer"
	Member Role = "member"
	Admin  Role = "admin"
	Owner  Role = "owner"
)

var roleRank = map[Role]int{Viewer: 0, Member: 1, Admin: 2, Owner: 3}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { _, ok := roleRank[r]; return ok }

// AtLeast reports whether r grants at least least.
func (r Role) AtLeast(least Role) bool { return r.Valid() && roleRank[r] >= roleRank[least] }

// User is a person who can log in.
type User struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	PlatformAdmin bool      `json:"platform_admin"`
	CreatedAt     time.Time `json:"created_at"`
	// PasswordHash is the argon2id PHC string; never serialised.
	PasswordHash string `json:"-"`
}

// Org is a tenant.
type Org struct {
	ID        string    `json:"id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Membership is a user's role in an org.
type Membership struct {
	Org     Org       `json:"org"`
	User    User      `json:"user"`
	Role    Role      `json:"role"`
	AddedAt time.Time `json:"added_at"`
}

// Session is a login; only the token's hash is stored.
type Session struct {
	TokenHash string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// APIKey is a personal key bound to one org. The secret is shown once;
// only its hash and a short prefix for recognition are kept.
type APIKey struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	OrgID      string     `json:"org_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"` // "csk_ab12cd34"
	Hash       string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// Visibility of a repository entry.
const (
	Public  = "public"  // anyone may add it; its graph is shared
	Private = "private" // added with credentials; shared only after proof of access
	Local   = "local"   // a mounted folder (platform admins)
)

// Repository is an organisation's entry for a graph.
type Repository struct {
	OrgID      string    `json:"org_id"`
	Graph      string    `json:"graph"`
	Remote     string    `json:"remote,omitempty"`
	Visibility string    `json:"visibility"`
	AddedBy    string    `json:"added_by,omitempty"`
	AddedAt    time.Time `json:"added_at"`
}

// Event is an audit log entry. Entries form a hash chain (see audit.go):
// each Hash covers the entry and the previous entry's Hash, so a changed or
// removed entry is detectable.
type Event struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	RequestID  string    `json:"request_id,omitempty"`
	UserID     string    `json:"user_id,omitempty"`
	ActorEmail string    `json:"actor_email,omitempty"`
	Via        string    `json:"via,omitempty"` // session, key, service, anonymous
	KeyID      string    `json:"key_id,omitempty"`
	OrgID      string    `json:"org_id,omitempty"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	Action     string    `json:"action"`
	Target     string    `json:"target,omitempty"`
	Outcome    string    `json:"outcome"` // success or failure
	Status     int       `json:"status,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	PrevHash   string    `json:"prev_hash"`
	Hash       string    `json:"hash"`
}

// EventFilter selects audit entries. Zero values mean no bound; OrgID
// "" means every organisation (platform admins).
type EventFilter struct {
	OrgID   string
	Since   time.Time
	Until   time.Time
	AfterID int64
	Limit   int
	// Ascending orders oldest first (export, verification).
	Ascending bool
}

// Errors.
var (
	ErrNotFound     = errors.New("not found")
	ErrExists       = errors.New("already exists")
	ErrUnauthorized = errors.New("invalid credentials")
	ErrForbidden    = errors.New("not allowed")
	ErrSignupClosed = errors.New("sign-up is closed; ask an administrator for an account")
	ErrRateLimited  = errors.New("too many attempts; wait a minute and try again")
	ErrLastOwner    = errors.New("an organisation needs at least one owner")
	ErrInvalidInput = errors.New("invalid input")
	ErrKeyRevoked   = errors.New("API key revoked or expired")
)

// Store persists accounts. Implementations must be safe for concurrent use
// and behave identically; accountstest.Run is the contract.
type Store interface {
	// Migrate creates or upgrades the schema. It is idempotent.
	Migrate(ctx context.Context) error

	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, u User) error // ErrExists on a taken email
	UserByID(ctx context.Context, id string) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)
	SetPassword(ctx context.Context, userID, hash string) error
	ListUsers(ctx context.Context, limit int) ([]User, error)

	CreateOrg(ctx context.Context, o Org, owner string) error // ErrExists on a taken slug
	OrgBySlug(ctx context.Context, slug string) (Org, error)
	OrgByID(ctx context.Context, id string) (Org, error)
	ListOrgs(ctx context.Context, limit int) ([]Org, error)
	// Memberships of a user, by org name; or of an org, by user email.
	UserMemberships(ctx context.Context, userID string) ([]Membership, error)
	OrgMembers(ctx context.Context, orgID string) ([]Membership, error)
	MemberRole(ctx context.Context, orgID, userID string) (Role, error) // ErrNotFound when not a member
	SetMember(ctx context.Context, orgID, userID string, role Role) error
	RemoveMember(ctx context.Context, orgID, userID string) error

	CreateSession(ctx context.Context, s Session) error
	SessionByHash(ctx context.Context, hash string) (Session, error)
	DeleteSession(ctx context.Context, hash string) error
	DeleteUserSessions(ctx context.Context, userID string) error

	CreateAPIKey(ctx context.Context, k APIKey) error
	APIKeyByHash(ctx context.Context, hash string) (APIKey, error)
	ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error)
	RevokeAPIKey(ctx context.Context, userID, keyID string, at time.Time) error // ErrNotFound for others' keys
	TouchAPIKey(ctx context.Context, keyID string, at time.Time) error

	// AddRepository links a graph to an organisation (ErrExists if linked).
	AddRepository(ctx context.Context, r Repository) error
	// RemoveRepository unlinks it and returns how many organisations still
	// reference the graph.
	RemoveRepository(ctx context.Context, orgID, graph string) (int, error)
	OrgRepositories(ctx context.Context, orgID string) ([]Repository, error)
	// GraphRepositories lists every organisation's entry for a graph.
	GraphRepositories(ctx context.Context, graph string) ([]Repository, error)

	// AddEvent appends e to the audit chain: under a lock, it sets
	// e.PrevHash to the last entry's hash and e.Hash = e.ChainHash().
	AddEvent(ctx context.Context, e Event) (Event, error)
	ListEvents(ctx context.Context, f EventFilter) ([]Event, error)
	// PruneEvents deletes entries before t (retention).
	PruneEvents(ctx context.Context, before time.Time) (int, error)
}
