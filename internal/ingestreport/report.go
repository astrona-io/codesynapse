// Package ingestreport accounts for encountered files without treating parsing
// as proof that a graph write committed.
package ingestreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Report format and resource limits.
const (
	Version          = 1
	MaxFiles         = 10000
	MaxReportBytes   = 16 << 20
	ExtractorVersion = "codesynapse-ingest-v1"
)

// Status describes extraction and publication progress.
type Status string

// Ingest lifecycle states.
const (
	Queued    Status = "queued"
	Running   Status = "running"
	Prepared  Status = "prepared"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// GraphCommit distinguishes acknowledgement from a prepared graph.
type GraphCommit string

// Graph publication outcomes.
const (
	CommitNone      GraphCommit = "none"
	CommitPending   GraphCommit = "pending"
	CommitCommitted GraphCommit = "committed"
	CommitFailed    GraphCommit = "failed"
	CommitUnknown   GraphCommit = "unknown"
)

// Outcome is the disposition of one encountered file.
type Outcome string

// Per-file accounting outcomes.
const (
	Pending     Outcome = "pending"
	Indexed     Outcome = "indexed"
	Skipped     Outcome = "skipped"
	FileFailed  Outcome = "failed"
	Unprocessed Outcome = "unprocessed"
)

// SkipReason is a bounded whole-file exclusion reason.
type SkipReason string

// Whole-file skip reasons, ordered separately by precedence.
const (
	IgnoredPath       SkipReason = "ignored_path"
	GeneratedCode     SkipReason = "generated_code"
	Binary            SkipReason = "binary"
	NonKubernetesYAML SkipReason = "non_k8s_yaml"
)

// Path exclusions take precedence without requiring content classification.
var skipReasons = []SkipReason{IgnoredPath, GeneratedCode, Binary, NonKubernetesYAML}

// SkipReasons returns the precedence order without exposing mutable state.
func SkipReasons() []SkipReason { return slices.Clone(skipReasons) }

// Stage identifies a required extractor or scanner.
type Stage string

// Extractor stages that can contribute graph data.
const (
	Go           Stage = "go"
	Docs         Stage = "docs"
	Language     Stage = "language"
	Config       Stage = "config"
	Architecture Stage = "architecture"
	Dependencies Stage = "dependencies"
	Kubernetes   Stage = "kubernetes"
)

// StageOutcome records extraction independently from graph publication.
type StageOutcome string

// Stage execution outcomes.
const (
	StageSucceeded StageOutcome = "succeeded"
	StageFailed    StageOutcome = "failed"
	StageExcluded  StageOutcome = "excluded"
)

// StageDecision contains no parser error text or repository contents.
type StageDecision struct {
	Stage      Stage        `json:"stage"`
	Outcome    StageOutcome `json:"outcome"`
	Diagnostic string       `json:"diagnostic,omitempty"`
}

// File retains one decision per encountered repository-relative path.
type File struct {
	Path         string          `json:"path"`
	SizeBytes    *int64          `json:"size_bytes"`
	Outcome      Outcome         `json:"outcome"`
	SkipReason   SkipReason      `json:"skip_reason,omitempty"`
	Diagnostic   string          `json:"diagnostic,omitempty"`
	Stages       []StageDecision `json:"stages,omitempty"`
	Contribution bool            `json:"contribution"`
	Reused       bool            `json:"reused"`
}

// Estimate records the metadata-only baseline and its missing coverage.
type Estimate struct {
	Name                  string `json:"name"`
	Version               int    `json:"version"`
	BytesPerToken         int    `json:"bytes_per_token"`
	Baseline              string `json:"baseline"`
	ObservedMetadataBytes int64  `json:"observed_metadata_bytes"`
	CoveredFiles          int    `json:"covered_files"`
	UnknownFiles          int    `json:"unknown_files"`
	ExcludedBinaryFiles   int    `json:"excluded_binary_files"`
	Partial               bool   `json:"partial"`
	Explanation           string `json:"explanation"`
}

// Report is a bounded, tenant-owned ingest ledger snapshot.
type Report struct {
	Version                      int                `json:"version"`
	RunID                        string             `json:"run_id"`
	Revision                     string             `json:"revision,omitempty"`
	PolicyDigest                 string             `json:"policy_digest,omitempty"`
	ExtractorVersion             string             `json:"extractor_version"`
	StartedAt                    time.Time          `json:"started_at"`
	FinishedAt                   *time.Time         `json:"finished_at,omitempty"`
	Status                       Status             `json:"status"`
	GraphCommit                  GraphCommit        `json:"graph_commit"`
	TotalFilesScanned            int                `json:"total_files_scanned"`
	FilesIndexed                 int                `json:"files_indexed"`
	FilesWritten                 int                `json:"files_written"`
	FilesReused                  int                `json:"files_reused"`
	FilesSkipped                 int                `json:"files_skipped"`
	FilesFailed                  int                `json:"files_failed"`
	FilesUnprocessed             int                `json:"files_unprocessed"`
	SkipBreakdown                map[SkipReason]int `json:"skip_breakdown"`
	DiscoveryComplete            bool               `json:"discovery_complete"`
	PrunedDirectories            int                `json:"pruned_directories"`
	EstimatedTokensSavedOnIngest *int64             `json:"estimated_tokens_saved_on_ingest"`
	Estimate                     Estimate           `json:"estimate"`
	Files                        []File             `json:"files"`
}

func terminal(s Status) bool { return s == Succeeded || s == Failed || s == Cancelled }

// ValidPath accepts bounded repository-relative paths without traversal.
func ValidPath(s string) bool {
	return s != "" && len(s) <= 1024 && utf8.ValidString(s) && path.Clean(s) == s && s != "." && s != ".." && !strings.HasPrefix(s, "../") && !strings.HasPrefix(s, "/") && !strings.ContainsAny(s, "\\:\x00") && !strings.ContainsFunc(s, unicode.IsControl)
}
func validDiagnostic(s string) bool {
	switch s {
	case "", "ignored_path", "unsupported_file_class", "no_contribution", "symlink", "special_file", "metadata_unavailable", "size_limit", "read_failed", "parse_failed", "package_load_failed", "cancelled", "not_completed", "graph_write_failed", "graph_commit_unknown", "stage_excluded", "generated_code", "binary", "non_k8s_yaml":
		return true
	}
	return false
}

// Clone returns a snapshot with independent slices, maps and pointers.
func (r Report) Clone() Report {
	r.Files = slices.Clone(r.Files)
	for i := range r.Files {
		r.Files[i].Stages = slices.Clone(r.Files[i].Stages)
		if r.Files[i].SizeBytes != nil {
			n := *r.Files[i].SizeBytes
			r.Files[i].SizeBytes = &n
		}
	}
	original := r.SkipBreakdown
	r.SkipBreakdown = map[SkipReason]int{}
	for k, v := range original {
		r.SkipBreakdown[k] = v
	}
	if r.FinishedAt != nil {
		at := *r.FinishedAt
		r.FinishedAt = &at
	}
	if r.EstimatedTokensSavedOnIngest != nil {
		n := *r.EstimatedTokensSavedOnIngest
		r.EstimatedTokensSavedOnIngest = &n
	}
	return r
}

func (r Report) recount() Report {
	r.TotalFilesScanned = len(r.Files)
	r.FilesIndexed, r.FilesWritten, r.FilesReused, r.FilesSkipped, r.FilesFailed, r.FilesUnprocessed = 0, 0, 0, 0, 0, 0
	r.SkipBreakdown = map[SkipReason]int{}
	for _, f := range r.Files {
		switch f.Outcome {
		case Indexed:
			r.FilesIndexed++
			if f.Reused {
				r.FilesReused++
			} else {
				r.FilesWritten++
			}
		case Skipped:
			r.FilesSkipped++
			r.SkipBreakdown[f.SkipReason]++
		case FileFailed:
			r.FilesFailed++
		case Unprocessed:
			r.FilesUnprocessed++
		}
	}
	return r
}

// Finish can be replayed after reconciling a durable snapshot receipt. Unknown
// acknowledgement never counts contributions as indexed.
func (r Report) Finish(status Status, commit GraphCommit) (Report, error) {
	if err := r.Validate(); err != nil {
		return Report{}, err
	}
	if !terminal(status) || (status == Succeeded && r.Status != Prepared && r.Status != Succeeded) || (status == Succeeded && commit != CommitCommitted) || (status != Succeeded && commit == CommitCommitted) {
		return Report{}, errors.New("invalid ingest finalization")
	}
	if terminal(r.Status) {
		if r.Status != status || r.GraphCommit != commit {
			return Report{}, errors.New("ingest report already finalized")
		}
		return r.Clone(), nil
	}
	r = r.Clone()
	r.Status = status
	r.GraphCommit = commit
	now := time.Now().UTC()
	r.FinishedAt = &now
	for i := range r.Files {
		f := &r.Files[i]
		if f.Outcome == Skipped || f.Outcome == FileFailed {
			continue
		}
		if f.Contribution && commit == CommitCommitted {
			f.Outcome = Indexed
			f.Diagnostic = ""
			continue
		}
		if f.Contribution && commit == CommitFailed {
			f.Outcome = FileFailed
			f.Diagnostic = "graph_write_failed"
			continue
		}
		f.Outcome = Unprocessed
		if f.Diagnostic == "" {
			switch {
			case commit == CommitUnknown:
				f.Diagnostic = "graph_commit_unknown"
			case status == Cancelled:
				f.Diagnostic = "cancelled"
			default:
				f.Diagnostic = "not_completed"
			}
		}
	}
	r = r.recount()
	if err := r.Validate(); err != nil {
		return Report{}, err
	}
	return r, nil
}

// Validate refuses totals that do not match the individual file decisions.
func (r Report) Validate() error {
	if r.Version != Version || r.RunID == "" || len(r.RunID) > 128 || strings.ContainsFunc(r.RunID, unicode.IsControl) || r.StartedAt.IsZero() || r.ExtractorVersion != ExtractorVersion || len(r.Files) > MaxFiles || r.PrunedDirectories < 0 {
		return errors.New("invalid ingest report identity or bounds")
	}
	switch r.Status {
	case Queued, Running, Prepared, Succeeded, Failed, Cancelled:
	default:
		return errors.New("invalid ingest status")
	}
	validCommit := (r.Status == Queued || r.Status == Running) && r.GraphCommit == CommitNone || r.Status == Prepared && r.GraphCommit == CommitPending || r.Status == Succeeded && r.GraphCommit == CommitCommitted || (r.Status == Failed || r.Status == Cancelled) && (r.GraphCommit == CommitNone || r.GraphCommit == CommitFailed || r.GraphCommit == CommitUnknown)
	if !validCommit {
		return errors.New("invalid graph commit status")
	}
	if terminal(r.Status) != (r.FinishedAt != nil) || (r.FinishedAt != nil && r.FinishedAt.Before(r.StartedAt)) || (r.Status == Succeeded && r.GraphCommit != CommitCommitted) {
		return errors.New("invalid ingest completion")
	}
	seen := map[string]bool{}
	for _, f := range r.Files {
		if f.Reused && !f.Contribution {
			return errors.New("reused file lacks contribution")
		}
		if !ValidPath(f.Path) || seen[f.Path] || (f.SizeBytes != nil && *f.SizeBytes < 0) || !validDiagnostic(f.Diagnostic) || len(f.Stages) > 7 {
			return errors.New("invalid ingest file")
		}
		seen[f.Path] = true
		switch f.Outcome {
		case Pending:
			if terminal(r.Status) {
				return errors.New("terminal ingest contains pending file")
			}
		case Indexed:
			if r.GraphCommit != CommitCommitted || !f.Contribution || !hasSuccess(f.Stages) {
				return errors.New("indexed file lacks committed contribution")
			}
		case Skipped:
			if !slices.Contains(skipReasons, f.SkipReason) || f.Contribution {
				return errors.New("invalid whole-file skip")
			}
		case FileFailed, Unprocessed:
		default:
			return errors.New("invalid file outcome")
		}
		if f.Outcome != Skipped && f.SkipReason != "" {
			return errors.New("skip reason on non-skipped file")
		}
		stages := map[Stage]bool{}
		for _, s := range f.Stages {
			if !slices.Contains([]Stage{Go, Docs, Language, Config, Architecture, Dependencies, Kubernetes}, s.Stage) || stages[s.Stage] || !validDiagnostic(s.Diagnostic) {
				return errors.New("invalid file stage")
			}
			stages[s.Stage] = true
			if s.Outcome != StageSucceeded && s.Outcome != StageFailed && s.Outcome != StageExcluded {
				return errors.New("invalid stage outcome")
			}
			if f.Outcome == Indexed && s.Outcome == StageFailed {
				return errors.New("indexed file has failed extraction")
			}
		}
	}
	c := r.recount()
	if r.TotalFilesScanned != c.TotalFilesScanned || r.FilesIndexed != c.FilesIndexed || r.FilesWritten != c.FilesWritten || r.FilesReused != c.FilesReused || r.FilesSkipped != c.FilesSkipped || r.FilesFailed != c.FilesFailed || r.FilesUnprocessed != c.FilesUnprocessed {
		return errors.New("ingest totals do not match files")
	}
	for _, reason := range skipReasons {
		if r.SkipBreakdown[reason] != c.SkipBreakdown[reason] {
			return errors.New("ingest skip totals do not match files")
		}
	}
	for k := range r.SkipBreakdown {
		if !slices.Contains(skipReasons, k) {
			return errors.New("unknown ingest skip reason")
		}
	}
	if terminal(r.Status) && r.TotalFilesScanned != r.FilesIndexed+r.FilesSkipped+r.FilesFailed+r.FilesUnprocessed {
		return errors.New("terminal ingest totals incomplete")
	}
	expected := estimate(r.Files, r.DiscoveryComplete, r.PrunedDirectories)
	if r.Estimate != expected || (r.EstimatedTokensSavedOnIngest == nil) != (expected.CoveredFiles == 0) || (r.EstimatedTokensSavedOnIngest != nil && *r.EstimatedTokensSavedOnIngest != expected.ObservedMetadataBytes/4) {
		return errors.New("invalid context estimate")
	}
	if len(r.Revision) > 128 || len(r.PolicyDigest) > 128 || !utf8.ValidString(r.Revision) || !utf8.ValidString(r.PolicyDigest) || strings.ContainsFunc(r.Revision, unicode.IsControl) || strings.ContainsFunc(r.PolicyDigest, unicode.IsControl) {
		return errors.New("invalid source identity")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxReportBytes {
		return fmt.Errorf("ingest report exceeds %d bytes", MaxReportBytes)
	}
	return nil
}
