// Package accounts holds users, organisations (tenants), memberships,
// sessions and API keys, and the rules around them: password hashing,
// token generation, roles and who may do what. Storage is behind Store
// (PostgreSQL in production, memory in tests); see docs/SAAS.md.
package accounts

import (
	"context"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/ingestreport"
)

// Role is a user's role in an organisation, weakest first.
type Role string

// Roles. A guest belongs to the organisation but inherits no repository:
// it sees only what is granted to it (see authz.go).
const (
	None   Role = "none" // explicit repository deny; not an organisation role
	Guest  Role = "guest"
	Viewer Role = "viewer"
	Member Role = "member"
	Admin  Role = "admin"
	Owner  Role = "owner"
)

var roleRank = map[Role]int{Guest: 0, Viewer: 1, Member: 2, Admin: 3, Owner: 4}

// Max returns the stronger of two roles ("" is weaker than any role).
func Max(a, b Role) Role {
	if !b.Valid() || (a.Valid() && roleRank[a] >= roleRank[b]) {
		return a
	}
	return b
}

// Valid reports whether r is a known role.
func (r Role) Valid() bool { _, ok := roleRank[r]; return ok }

// AtLeast reports whether r grants at least least.
func (r Role) AtLeast(least Role) bool {
	return r.Valid() && least.Valid() && roleRank[r] >= roleRank[least]
}

// User is a person who can log in.
type User struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	PlatformAdmin bool   `json:"platform_admin"`
	// BootstrapRoot marks the account that set up the installation. It is
	// fixed when the account is created or designated and never cleared:
	// unlike PlatformAdmin it cannot be granted or revoked. Root may do
	// governance only (see bootstrap.go).
	BootstrapRoot bool      `json:"bootstrap_root,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	// DisabledAt is set while the account is suspended: no sign-in, and
	// its sessions and API keys stop working.
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	// PasswordHash is the argon2id PHC string; never serialised.
	PasswordHash string `json:"-"`
}

// Default repository visibility policies.
const (
	DefaultRepoNone     = "NONE"
	DefaultRepoReadOnly = "READ_ONLY"
)

// Org is a tenant.
type Org struct {
	DefaultRepoAccess string    `json:"default_repo_access"`
	PolicyRevision    int64     `json:"policy_revision"`
	ID                string    `json:"id"`
	Slug              string    `json:"slug"`
	Name              string    `json:"name"`
	CreatedAt         time.Time `json:"created_at"`
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
	// UserAgent is the browser that signed in, so a person can tell their
	// sessions apart (clipped; no address is kept).
	UserAgent string
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
	// Access is AccessOK, or AccessLost once the organisation's own token
	// stopped reading a private repository; lost entries are invisible to
	// the organisation until a working token restores them.
	Access     string     `json:"access"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// Repository access states.
const (
	AccessOK   = "ok"
	AccessLost = "lost"
)

// Usable reports whether the organisation may use the entry's graph.
func (r Repository) Usable() bool { return r.Access != AccessLost }

// Team groups members of an organisation; roles granted to a team apply to
// its members.
type Team struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Tuple is a relationship in Zanzibar notation: object#relation@subject,
// where the subject is a user ("user:U") or every holder of a relation on
// another object ("team:T#member"). Organisation memberships stay in their
// own table and act as organisation:O#<role>@user:U tuples.
type Tuple struct {
	ObjectType      string    `json:"object_type"`
	ObjectID        string    `json:"object_id"`
	Relation        string    `json:"relation"`
	SubjectType     string    `json:"subject_type"`
	SubjectID       string    `json:"subject_id"`
	SubjectRelation string    `json:"subject_relation,omitempty"`
	CreatedBy       string    `json:"created_by,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// TupleFilter selects tuples; empty fields match anything.
type TupleFilter struct {
	ObjectType, ObjectID, Relation          string
	SubjectType, SubjectID, SubjectRelation string
	// SubjectIDs, when set, matches any of these subject IDs.
	SubjectIDs []string
	ObjectIDs  []string
}

// GitToken is an organisation's credential for one Git host. The token is
// stored sealed (AES-256-GCM, see Sealer) and only opened to clone or
// fetch; the API shows Preview, never the token.
type GitToken struct {
	OrgID     string    `json:"org_id"`
	Host      string    `json:"host"`
	Preview   string    `json:"preview"`
	KeyID     string    `json:"-"`
	Sealed    []byte    `json:"-"`
	CreatedBy string    `json:"created_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
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
	// MCP describes an MCP request (docs/SAAS.md, 5c); nil for everything else.
	MCP      *MCPRequest `json:"mcp,omitempty"`
	PrevHash string      `json:"prev_hash"`
	Hash     string      `json:"hash"`
}

// MCPRequest is what the audit log keeps about one MCP request: enough to
// trace it back to a person, key, chat and client, never the results. Args
// holds identifying values and redacted, capped free text; bulk inputs (a
// diff, code, conversation turns) appear only as "sha256:<hex> (<n> bytes)".
// ArgsDigest is SHA-256 over the canonical arguments, so a payload found
// later can be matched to its request.
type MCPRequest struct {
	Method      string            `json:"method"`
	Name        string            `json:"name,omitempty"` // tool or prompt
	Repo        string            `json:"repo,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`   // runtime (chat) session
	TransportID string            `json:"transport_id,omitempty"` // MCP transport session
	Client      string            `json:"client,omitempty"`       // name and version from initialize
	DurationMS  int64             `json:"duration_ms"`
	Bytes       int64             `json:"bytes"`
	ArgsDigest  string            `json:"args_digest,omitempty"`
	Args        map[string]string `json:"args,omitempty"`
	Error       string            `json:"error,omitempty"`
}

// EventFilter selects audit entries. Zero values mean no bound; OrgID
// "" means every organisation (platform admins).
type EventFilter struct {
	OrgID   string
	Since   time.Time
	Until   time.Time
	AfterID int64
	Limit   int
	// Action matches actions starting with it ("user." finds every user
	// event); Actor matches the actor's email or user ID; Outcome is
	// success, failure or empty for both.
	Action, Actor, Outcome string
	// Session, Tool and Repo match MCP requests by runtime session, tool or
	// prompt name and repository.
	Session, Tool, Repo string
	// Ascending orders oldest first (export, verification).
	Ascending bool
}

// Errors.
var (
	ErrAccessLimit   = errors.New("access policy exceeds evaluation limit")
	ErrLastRepoAdmin = errors.New("a repository needs at least one active administrator")
	ErrNotFound      = errors.New("not found")
	ErrExists        = errors.New("already exists")
	ErrUnauthorized  = errors.New("invalid credentials")
	ErrForbidden     = errors.New("not allowed")
	ErrSignupClosed  = errors.New("sign-up is closed; ask an administrator for an account")
	ErrRateLimited   = errors.New("too many attempts; wait a minute and try again")
	ErrLastOwner     = errors.New("an organisation needs at least one owner")
	ErrInvalidInput  = errors.New("invalid input")
	ErrKeyRevoked    = errors.New("API key revoked or expired")
	ErrSuspended     = errors.New("this account is suspended; ask an administrator")
	ErrNoSecretKey   = errors.New("organisation Git tokens need CODESYNAPSE_SECRET_KEY (32 random bytes, base64 or hex) on the API")
	// ErrRootAccount denies business work to the bootstrap root account.
	ErrRootAccount error = rootAccountError{}
	// ErrSetupComplete refuses a second bootstrap.
	ErrSetupComplete = errors.New("setup is already complete")
	// ErrSetupRequired refuses work that needs the installation set up.
	ErrSetupRequired = errors.New("finish setup first: create an organisation and its owner")
)

type rootAccountError struct{}

// Error is shown to people as is, so it is a full sentence.
func (rootAccountError) Error() string {
	return "Root accounts cannot perform organisation work. Sign out and sign in with a standard account in your organisation."
}

// Store persists accounts. Implementations must be safe for concurrent use
// and behave identically; accountstest.Run is the contract.
type Store interface {
	// Migrate creates or upgrades the schema. It is idempotent.
	Migrate(ctx context.Context) error

	// PolicyTransaction serializes policy changes and commits their audit/outbox records together.
	PolicyTransaction(ctx context.Context, orgID string, fn func(Store) error) error
	// ReadSnapshot evaluates current policy consistently without serializing other readers.
	ReadSnapshot(ctx context.Context, fn func(Store) error) error
	SetDefaultRepoAccess(ctx context.Context, orgID, policy string) error
	TouchPolicy(ctx context.Context, orgID string) error
	PendingAccessPolicies(ctx context.Context, limit int) ([]Org, error)
	AcknowledgeAccessPolicy(ctx context.Context, orgID string, revision int64) error

	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, u User) error // ErrExists on a taken email; ErrExists for a second bootstrap root
	// MarkBootstrapRoot classifies an existing account as the bootstrap
	// root (migration and explicit designation). ErrExists when another
	// account already is; the classification is never removed.
	MarkBootstrapRoot(ctx context.Context, userID string) error
	// Setup returns the installation's setup record; State is empty before
	// the first account exists.
	Setup(ctx context.Context) (Setup, error)
	// SaveSetup writes the single setup record.
	SaveSetup(ctx context.Context, s Setup) error

	// Single sign-on (sso.go): one provider per organisation, keyed by org.
	PutIdentityProvider(ctx context.Context, p IdentityProvider) error
	IdentityProvider(ctx context.Context, orgID string) (IdentityProvider, error) // ErrNotFound
	DeleteIdentityProvider(ctx context.Context, orgID string) error
	IdentityBySubject(ctx context.Context, issuer, subject string) (ExternalIdentity, error) // ErrNotFound
	// PutIdentity creates or updates by ID; ErrExists when another ID holds
	// the issuer and subject.
	PutIdentity(ctx context.Context, e ExternalIdentity) error
	UserIdentities(ctx context.Context, userID string) ([]ExternalIdentity, error)
	DeleteIdentity(ctx context.Context, userID, id string) error
	PutSSOState(ctx context.Context, s SSOState) error
	// TakeSSOState returns and deletes a state, so it works once.
	TakeSSOState(ctx context.Context, stateHash string) (SSOState, error)
	PruneSSOStates(ctx context.Context, now time.Time) error
	// ManagedByIDP reports whether the organisation's provider created the
	// membership; SetManagedByIDP marks or clears it.
	ManagedByIDP(ctx context.Context, orgID, userID string) (bool, error)
	SetManagedByIDP(ctx context.Context, orgID, userID string, managed bool) error
	UserByID(ctx context.Context, id string) (User, error)
	UserByEmail(ctx context.Context, email string) (User, error)
	SetPassword(ctx context.Context, userID, hash string) error
	ListUsers(ctx context.Context, limit int) ([]User, error)
	// SetUserDisabled suspends (at != nil) or restores a user.
	SetUserDisabled(ctx context.Context, userID string, at *time.Time) error
	SetPlatformAdmin(ctx context.Context, userID string, admin bool) error

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
	// UserSessions lists a user's unexpired sessions, newest first, at most 100.
	UserSessions(ctx context.Context, userID string) ([]Session, error)
	// DeleteOtherSessions ends every session of the user except keepHash.
	DeleteOtherSessions(ctx context.Context, userID, keepHash string) error
	SetUserName(ctx context.Context, userID, name string) error
	// SetUserEmail fails with ErrExists when another account has the email.
	SetUserEmail(ctx context.Context, userID, email string) error

	// Execution sessions retain closed rows permanently to reject replay.
	// Schedules (schedules.go). EnsureSchedule adds one only when the
	// repository has none. DueSchedules lists repositories whose next run
	// has passed. RecordScheduleRun and AddScheduleMissing change only
	// their fields. TakeLease gives owner the named lease until until when
	// it is free, expired or already owner's.
	ScheduleByRepo(ctx context.Context, repo string) (Schedule, error)
	PutSchedule(ctx context.Context, sc Schedule) error
	EnsureSchedule(ctx context.Context, sc Schedule) error
	DeleteSchedule(ctx context.Context, repo string) error
	DueSchedules(ctx context.Context, now time.Time) ([]string, error)
	RecordScheduleRun(ctx context.Context, repo string, run ScheduleRun) error
	AddScheduleMissing(ctx context.Context, repo string, gone bool) (int, error)
	TakeLease(ctx context.Context, name, owner string, until, now time.Time) (bool, error)
	// TakeAttempt records an attempt for key at now unless key already has
	// limit attempts after since, and reports whether it was recorded.
	// Attempts at or before since, of any key, may be deleted. Every API
	// replica shares the count (login limits, docs/SAAS.md 6c).
	TakeAttempt(ctx context.Context, key string, limit int, since, now time.Time) (bool, error)
	// NotificationsSeen is when the user last marked the dashboard's
	// notifications read (zero when never). SetNotificationsSeen only moves
	// it forward.
	NotificationsSeen(ctx context.Context, userID string) (time.Time, error)
	SetNotificationsSeen(ctx context.Context, userID string, at time.Time) error
	// Durable jobs (jobs.go). AddJob refuses a second active job for the
	// same repository (ErrJobActive). RenewJobLeases extends every active
	// job owner holds. FinishJob ends a job owner still holds and clears its
	// parameters. ClaimOrphanedJob hands owner the oldest active job whose
	// lease expired (a stopped process's, or one queued for workers),
	// failing those that ran out of attempts on the way.
	AddJob(ctx context.Context, job JobRecord) error
	JobByID(ctx context.Context, id string) (JobRecord, error)
	ListJobs(ctx context.Context, orgIDs []string, limit int) ([]JobRecord, error)
	StartJob(ctx context.Context, id, owner string, leaseUntil, now time.Time) error
	RenewJobLeases(ctx context.Context, owner string, until time.Time) error
	FinishJob(ctx context.Context, id, owner string, out JobOutcome) error
	// kinds limits the claim to those job kinds; nil claims any.
	ClaimOrphanedJob(ctx context.Context, owner string, kinds []string, leaseUntil, now time.Time) (JobRecord, bool, error)
	RegisterExecutionSession(ctx context.Context, session ExecutionSession) (ExecutionSession, bool, error)
	ExecutionSessionByID(ctx context.Context, id string) (ExecutionSession, error)
	RevokeExecutionSession(ctx context.Context, id, orgID, userID string, at time.Time) error
	// ClosedExecutionSessions pages permanent tombstones and expired bindings for memory cleanup.
	ClosedExecutionSessions(ctx context.Context, before time.Time, afterID string, limit int) ([]ExecutionSession, error)

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
	RepositoriesForOrgs(ctx context.Context, orgIDs []string) ([]Repository, error)
	// GraphRepositories lists every organisation's entry for a graph.
	GraphRepositories(ctx context.Context, graph string) ([]Repository, error)
	// SetRepositoryAccess records the outcome of an access check.
	SetRepositoryAccess(ctx context.Context, orgID, graph, access string, at time.Time) error
	GetRepoPolicy(ctx context.Context, orgID, graph string) (RepoPolicyApproval, error)
	PutRepoPolicy(ctx context.Context, approval RepoPolicyApproval) error
	DeleteRepoPolicy(ctx context.Context, orgID, graph string) error
	UpsertAnnotations(ctx context.Context, records []RepositoryAnnotation) error
	ListAnnotations(ctx context.Context, orgID, graph string, limit int) ([]RepositoryAnnotation, error)
	BeginIngestRun(ctx context.Context, run IngestRun) error
	SaveIngestRun(ctx context.Context, run IngestRun) error
	ReconcileIngestRun(ctx context.Context, orgID, graph, runID string, commit ingestreport.GraphCommit) error
	GetIngestRun(ctx context.Context, orgID, graph, runID string) (IngestRun, error)
	ListIngestRuns(ctx context.Context, orgID, graph string, limit int) ([]IngestRun, error)
	LatestSuccessfulIngestRun(ctx context.Context, orgID, graph string) (IngestRun, error)
	PendingIngestRuns(ctx context.Context, graph string, limit int) ([]IngestRun, error)
	ExportIngestTotals(ctx context.Context, export func(context.Context, IngestMetricSnapshot) error) error

	CreateTeam(ctx context.Context, t Team) error // ErrExists on a taken slug in the organisation
	TeamByID(ctx context.Context, id string) (Team, error)
	TeamBySlug(ctx context.Context, orgID, slug string) (Team, error)
	OrgTeams(ctx context.Context, orgID string) ([]Team, error)
	TeamsByIDs(ctx context.Context, ids []string) ([]Team, error)
	// DeleteTeam removes the team and every tuple naming it.
	DeleteTeam(ctx context.Context, id string) error

	// WriteTuple adds a relationship (ErrExists when present).
	WriteTuple(ctx context.Context, t Tuple) error
	DeleteTuple(ctx context.Context, t Tuple) error // ErrNotFound
	// ReadTuples lists matching relationships (at most MaxTuples).
	ReadTuples(ctx context.Context, f TupleFilter) ([]Tuple, error)
	// DeleteTuples removes every matching relationship and returns how many.
	DeleteTuples(ctx context.Context, f TupleFilter) (int, error)

	// PutGitToken inserts or replaces an organisation's token for a host.
	PutGitToken(ctx context.Context, t GitToken) error
	GitToken(ctx context.Context, orgID, host string) (GitToken, error) // ErrNotFound
	GitTokens(ctx context.Context, orgID string) ([]GitToken, error)    // by host
	DeleteGitToken(ctx context.Context, orgID, host string) error       // ErrNotFound

	// AddEvent appends e to the audit chain: under a lock, it sets
	// e.PrevHash to the last entry's hash and e.Hash = e.ChainHash().
	AddEvent(ctx context.Context, e Event) (Event, error)
	ListEvents(ctx context.Context, f EventFilter) ([]Event, error)
	// PruneEvents deletes entries before t (retention).
	PruneEvents(ctx context.Context, before time.Time) (int, error)
}
