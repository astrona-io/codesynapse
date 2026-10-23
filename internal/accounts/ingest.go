package accounts

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
)

// Lifetime admission bounds retain tombstones rather than rotating metric IDs.
const (
	IngestHistory                     = 50
	MaxIngestRuns                     = 100000
	MaxTenantIngestRuns               = 8192
	MaxRepositoryIngestRuns           = 2048
	MaxPendingIngestRuns              = 16
	MaxTenantPendingIngestRuns        = 4
	MaxIngestMetricRepositories       = 800
	MaxTenantIngestMetricRepositories = 64
)

// Accounting failures never authorize a graph write or invent a session.
var (
	ErrIngestLimit         = errors.New("ingestion accounting capacity reached")
	ErrIngestConflict      = errors.New("ingestion run identity or transition conflicts with its stored report")
	ErrIngestReportExpired = errors.New("ingestion report expired; its run identity remains reserved")
	ErrIngestExporterBusy  = errors.New("ingestion metrics exporter is already active")
)

// IngestRun is tenant-owned accounting. Runtime identity is never serialized in
// API reports; adapters store it separately for write validation and recovery.
type IngestRun struct {
	OrgID          string              `json:"org_id"`
	Graph          string              `json:"graph"`
	Identity       execution.Identity  `json:"-"`
	Report         ingestreport.Report `json:"report"`
	RepositoryID   string              `json:"repository_id"`
	MetricCoverage bool                `json:"metric_coverage"`
	LastExportedAt time.Time           `json:"last_exported_at,omitempty"`
}

// IngestMetricTotal is one cumulative six-series repository stream.
type IngestMetricTotal struct {
	TenantID     string
	RepositoryID string
	Scanned      int64
	Written      int64
	Skipped      map[ingestreport.SkipReason]int64
	StartedAt    time.Time
}

// IngestMetricSnapshot has persistent resource identity and sample timestamps.
type IngestMetricSnapshot struct {
	StreamID       string
	StartedAt      time.Time
	SampleAt       time.Time
	LastExportedAt time.Time
	DroppedSeries  int64
	Totals         []IngestMetricTotal
}

// NewIngestMetricID creates server-owned accounting IDs, never chat sessions.
func NewIngestMetricID() (string, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

// IngestTerminal separates durable final counts from discovery checkpoints.
func IngestTerminal(status ingestreport.Status) bool {
	return status == ingestreport.Succeeded || status == ingestreport.Failed || status == ingestreport.Cancelled
}

// PrepareIngestRun validates and copies the report before storing any state.
func PrepareIngestRun(ctx context.Context, run IngestRun) (IngestRun, string, error) {
	id, err := execution.Require(ctx)
	if err != nil {
		return IngestRun{}, "", err
	}
	if id != run.Identity || run.OrgID != id.TenantID || !policyText(run.Graph, 512) {
		return IngestRun{}, "", ErrIngestConflict
	}
	if err := run.Report.Validate(); err != nil {
		return IngestRun{}, "", err
	}
	if run.Report.GraphCommit == ingestreport.CommitUnknown {
		return IngestRun{}, "", ErrIngestConflict
	}
	b, err := json.Marshal(run.Report)
	if err != nil {
		return IngestRun{}, "", err
	}
	hash := sha256.Sum256(b)
	run.Report = run.Report.Clone()
	return run, hex.EncodeToString(hash[:]), nil
}

// CheckIngestAuthority runs inside the accounts policy transaction.
func CheckIngestAuthority(ctx context.Context, st Store, run IngestRun) error {
	service := NewService(st, false)
	if _, err := service.ResolveExecutionSession(ctx, run.Identity.SubjectID, run.OrgID, run.Identity.SessionID); err != nil {
		return err
	}
	u, err := st.UserByID(ctx, run.Identity.SubjectID)
	if err != nil {
		return err
	}
	_, err = service.ResolveRepository(ctx, Principal{User: u, Via: "session", OrgID: run.OrgID}, run.OrgID, run.Graph, Admin)
	return err
}

// CheckIngestTransition keeps checkpoint identity stable and prepared files
// immutable. Failure accounting may refine outcomes after a writer stops.
func CheckIngestTransition(previous, next IngestRun) error {
	a, b := previous.Report, next.Report
	if previous.Identity != next.Identity || previous.OrgID != next.OrgID || previous.Graph != next.Graph || a.RunID != b.RunID || !a.StartedAt.Equal(b.StartedAt) || a.ExtractorVersion != b.ExtractorVersion || (a.Revision != "" && a.Revision != b.Revision) || (a.PolicyDigest != "" && a.PolicyDigest != b.PolicyDigest) {
		return ErrIngestConflict
	}
	if IngestTerminal(a.Status) {
		return ErrIngestConflict
	}
	if b.Status == ingestreport.Queued || (a.Status == ingestreport.Prepared && !IngestTerminal(b.Status)) {
		return ErrIngestConflict
	}
	if b.Status == ingestreport.Succeeded && a.Status != ingestreport.Prepared {
		return ErrIngestConflict
	}
	if len(a.Files) > len(b.Files) {
		return ErrIngestConflict
	}
	files := make(map[string]ingestreport.File, len(b.Files))
	for _, f := range b.Files {
		files[f.Path] = f
	}
	for _, old := range a.Files {
		f, ok := files[old.Path]
		if !ok || (old.Outcome == ingestreport.Skipped && (f.Outcome != old.Outcome || f.SkipReason != old.SkipReason)) {
			return ErrIngestConflict
		}
		if (old.SizeBytes == nil) != (f.SizeBytes == nil) || (old.SizeBytes != nil && *old.SizeBytes != *f.SizeBytes) {
			return ErrIngestConflict
		}
		if old.Outcome == ingestreport.FileFailed && f.Outcome != ingestreport.FileFailed {
			return ErrIngestConflict
		}
		if a.Status == ingestreport.Prepared && (old.Contribution != f.Contribution || old.Reused != f.Reused || !slices.Equal(old.Stages, f.Stages)) {
			return ErrIngestConflict
		}
	}
	if a.Status == ingestreport.Prepared && len(a.Files) != len(b.Files) {
		return ErrIngestConflict
	}
	return nil
}

// FinishIngestRecovery uses only a persisted checkpoint and a caller-verified
// backend receipt. It performs accounting without reviving the old principal.
func FinishIngestRecovery(run IngestRun, commit ingestreport.GraphCommit) (IngestRun, error) {
	if commit != ingestreport.CommitCommitted && commit != ingestreport.CommitFailed && commit != ingestreport.CommitUnknown {
		return IngestRun{}, ErrIngestConflict
	}
	if commit == ingestreport.CommitCommitted && run.Report.Status != ingestreport.Prepared {
		return IngestRun{}, ErrIngestConflict
	}
	status := ingestreport.Failed
	if commit == ingestreport.CommitCommitted {
		status = ingestreport.Succeeded
	}
	report, err := run.Report.Finish(status, commit)
	run.Report = report
	return run, err
}
