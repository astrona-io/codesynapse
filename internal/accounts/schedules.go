package accounts

import (
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// Schedules (docs/SAAS.md, 5e): a repository's auto-sync settings and last
// run, shared by every API process instead of a file in one process's
// workspace. Runs and the missing-repository count are updated on their own,
// so the scheduler and a person changing the settings never overwrite each
// other.

// Schedule is one repository's auto-sync setting. NextRun is zero when no
// run is due (manual or paused).
type Schedule struct {
	Repo            string
	OrgID           string
	ActorID         string
	Execution       execution.Identity
	IntervalMinutes int
	Paused          bool
	AutoReview      bool
	AutoTriage      bool
	LastRun         time.Time
	LastStatus      string
	LastError       string
	Missing         int
	NextRun         time.Time
}

// ScheduleRun records how a scheduled run went and when the next is due.
type ScheduleRun struct {
	// LastRun is left as it was when zero (a run ending, not starting).
	LastRun    time.Time
	LastStatus string
	LastError  string
	// NextRun is left as it was when zero.
	NextRun time.Time
	// ResetMissing clears the missing-repository count.
	ResetMissing bool
}

// MaxSchedules bounds one listing of due schedules.
const MaxSchedules = 1000
