package accounts

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// Durable jobs (docs/SAAS.md, 5e): every clone, sync, ingest, AI task and fix
// is a row before it runs, so a restart neither loses queued work nor
// forgets what ran. A record holds who asked and how to rebuild the work,
// never credentials: tokens are derived again when the job runs, and the
// requester's session, membership and permission are checked again then.
// A process owns a job through a lease it renews while the job waits or
// runs; a job whose lease expired belongs to a process that stopped and
// can be claimed by another.

// Job states.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// Job limits.
const (
	// MaxJobAttempts bounds how often an interrupted job starts again.
	MaxJobAttempts = 3
	// MaxJobParams bounds the stored parameters (an AI review's diff is the
	// largest).
	MaxJobParams = 2 << 20
	// JobHistory is how many finished jobs are kept.
	JobHistory = 5000
)

// ErrJobActive means the repository already has a queued or running job.
var ErrJobActive = errors.New("the repository already has a queued or running job")

// JobRecord is one durable job.
type JobRecord struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Repo  string `json:"repo"`
	OrgID string `json:"org_id"`
	// ActorID, Via and KeyID are who asked and how they authenticated;
	// Role is the least repository role the job needs.
	ActorID   string             `json:"actor_id"`
	Via       string             `json:"via"`
	KeyID     string             `json:"key_id,omitempty"`
	Role      Role               `json:"role"`
	Execution execution.Identity `json:"execution"`
	// Params rebuild the work; cleared when the job ends.
	Params     json.RawMessage `json:"-"`
	Branch     string          `json:"branch,omitempty"`
	Task       string          `json:"task,omitempty"`
	State      string          `json:"state"`
	Attempts   int             `json:"attempts"`
	LeaseOwner string          `json:"-"`
	LeaseUntil time.Time       `json:"-"`
	CreatedAt  time.Time       `json:"created_at"`
	StartedAt  *time.Time      `json:"started_at,omitempty"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Result     string          `json:"result,omitempty"`
	Error      string          `json:"error,omitempty"`
	Nodes      int             `json:"nodes,omitempty"`
	Edges      int             `json:"edges,omitempty"`
}

// Active reports whether the job is queued or running.
func (j JobRecord) Active() bool { return j.State == JobQueued || j.State == JobRunning }

// ValidateJob checks a new record before a store admits it.
func ValidateJob(j JobRecord) error {
	if j.ID == "" || len(j.ID) > 64 || j.Kind == "" || len(j.Kind) > 32 || j.Repo == "" || len(j.Repo) > 300 ||
		j.OrgID == "" || j.ActorID == "" || !j.Role.Valid() || len(j.Params) > MaxJobParams ||
		j.State != JobQueued || j.LeaseOwner == "" || j.CreatedAt.IsZero() {
		return ErrInvalidInput
	}
	if err := execution.Validate(j.Execution); err != nil || j.Execution.TenantID != j.OrgID || j.Execution.SubjectID != j.ActorID {
		return ErrInvalidInput
	}
	return nil
}

// JobOutcome is how a job ended.
type JobOutcome struct {
	State        string
	Result       string
	Error        string
	Nodes, Edges int
	At           time.Time
}
