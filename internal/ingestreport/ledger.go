package ingestreport

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
)

// Ledger tracks discovery and extraction before a caller publishes the graph.
type Ledger struct {
	mu         sync.Mutex
	report     Report
	files      map[string]int
	discovered bool
	checkpoint func(context.Context, Report) error
}
type ledgerKey struct{}

// WithLedger shares the run ledger with existing extractor contexts.
func WithLedger(ctx context.Context, l *Ledger) context.Context {
	return context.WithValue(ctx, ledgerKey{}, l)
}

// FromContext returns nil for callers that do not request an ingest report.
func FromContext(ctx context.Context) *Ledger { l, _ := ctx.Value(ledgerKey{}).(*Ledger); return l }

// New starts a ledger for a caller-supplied run identifier.
func New(runID string, started time.Time) (*Ledger, error) {
	r := Report{Version: Version, RunID: runID, StartedAt: started.UTC(), Status: Queued, GraphCommit: CommitNone, ExtractorVersion: ExtractorVersion, SkipBreakdown: map[SkipReason]int{}, Files: []File{}, Estimate: estimate(nil, false, 0)}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &Ledger{report: r, files: map[string]int{}}, nil
}

// SetCheckpoint installs persistence before content extraction starts.
func (l *Ledger) SetCheckpoint(fn func(context.Context, Report) error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.checkpoint = fn
}

// Checkpoint persists a detached snapshot and propagates storage failures.
func (l *Ledger) Checkpoint(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	fn := l.checkpoint
	r := l.snapshot()
	l.mu.Unlock()
	if fn != nil {
		return fn(ctx, r)
	}
	return nil
}

// SetSource pins the revision and effective policy for this run.
func (l *Ledger) SetSource(revision, digest string) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.report.Revision != "" && l.report.Revision != revision || l.report.PolicyDigest != "" && l.report.PolicyDigest != digest {
		return errors.New("ingest source identity changed")
	}
	l.report.Revision, l.report.PolicyDigest = revision, digest
	return nil
}

// Discover reads directory entries and size metadata only. Ignored subtrees
// are pruned; their unknown children never enter the file totals or estimate.
func (l *Ledger) Discover(ctx context.Context, root string, policy *repoconfig.Policy) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.discovered {
		l.mu.Unlock()
		return errors.New("ingest discovery already completed")
	}
	l.discovered = true
	l.report.Status = Running
	l.report.DiscoveryComplete = true
	l.mu.Unlock()
	err := filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			l.incomplete()
			return nil
		} //nolint:nilerr // unknown entries are reported as incomplete discovery
		if filename == root {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			nested := false
			if _, err := os.Lstat(filepath.Join(filename, ".git")); err == nil {
				nested = true
			}
			if entry.Name() == ".git" || policy.PruneDir(rel) || nested {
				l.mu.Lock()
				l.report.PrunedDirectories++
				l.mu.Unlock()
				return filepath.SkipDir
			}
			return nil
		}
		if !ValidPath(rel) {
			l.incomplete()
			return nil
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		if len(l.report.Files) >= MaxFiles {
			l.report.DiscoveryComplete = false
			return filepath.SkipAll
		}
		f := File{Path: rel, Outcome: Pending}
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			f.Outcome = Unprocessed
			f.Diagnostic = "symlink"
		case !entry.Type().IsRegular():
			f.Outcome = Unprocessed
			f.Diagnostic = "special_file"
		default:
			info, err := entry.Info()
			if err != nil {
				f.Outcome = Unprocessed
				f.Diagnostic = "metadata_unavailable"
			} else {
				size := info.Size()
				f.SizeBytes = &size
			}
			if !policy.PathAllowed(rel) {
				f.Outcome = Skipped
				f.SkipReason = IgnoredPath
				f.Diagnostic = "ignored_path"
			}
		}
		l.files[rel] = len(l.report.Files)
		l.report.Files = append(l.report.Files, f)
		return nil
	})
	if err != nil {
		l.incomplete()
	}
	return err
}
func (l *Ledger) incomplete() { l.mu.Lock(); l.report.DiscoveryComplete = false; l.mu.Unlock() }

// Observe records fixed stage decisions only; it does not infer publication.
func Observe(ctx context.Context, rel string, stage Stage, outcome StageOutcome, diagnostic string) {
	l := FromContext(ctx)
	if l == nil {
		return
	}
	l.observe(rel, stage, outcome, diagnostic, false)
}

// Contribute is for successful extraction stored on a repository aggregate,
// such as a dependency manifest whose modules have no individual File node.
func Contribute(ctx context.Context, rel string, stage Stage) {
	l := FromContext(ctx)
	if l == nil {
		return
	}
	l.observe(rel, stage, StageSucceeded, "", true)
}
func (l *Ledger) observe(rel string, stage Stage, outcome StageOutcome, diagnostic string, contribution bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if terminal(l.report.Status) || l.report.Status == Prepared {
		return
	}
	index, ok := l.files[rel]
	if !ok {
		l.report.DiscoveryComplete = false
		return
	}
	f := &l.report.Files[index]
	if f.Outcome == Skipped {
		return
	}
	decision := StageDecision{Stage: stage, Outcome: outcome, Diagnostic: diagnostic}
	found := false
	for i, previous := range f.Stages {
		if previous.Stage == stage {
			found = true
			if previous.Outcome != StageFailed {
				f.Stages[i] = decision
			}
			break
		}
	}
	if !found {
		f.Stages = append(f.Stages, decision)
	}
	if outcome == StageFailed {
		f.Outcome = FileFailed
		f.Diagnostic = diagnostic
	}
	if contribution {
		f.Contribution = true
	}
}

// Skip accepts an already justified whole-file exclusion. Current extractors
// only classify ignored paths; generated/binary content is never read to invent savings.
func Skip(ctx context.Context, rel string, reason SkipReason) {
	l := FromContext(ctx)
	if l == nil || !slices.Contains(skipReasons, reason) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	index, ok := l.files[rel]
	if !ok || terminal(l.report.Status) || l.report.Status == Prepared {
		return
	}
	f := &l.report.Files[index]
	if f.Contribution || f.Outcome == FileFailed || hasSuccess(f.Stages) {
		return
	}
	if f.SkipReason != "" && slices.Index(skipReasons, f.SkipReason) < slices.Index(skipReasons, reason) {
		return
	}
	f.Outcome, f.SkipReason, f.Diagnostic = Skipped, reason, string(reason)
}

// Prepare reconciles successful extraction with actual graph contributions.
func (l *Ledger) Prepare(g *model.Graph) Report {
	l.mu.Lock()
	defer l.mu.Unlock()
	if terminal(l.report.Status) || l.report.Status == Prepared {
		return l.snapshot()
	}
	if g != nil {
		for _, node := range g.Nodes() {
			if node.File == "" {
				continue
			}
			index, ok := l.files[node.File]
			if !ok {
				l.report.DiscoveryComplete = false
				continue
			}
			file := &l.report.Files[index]
			// Findings without successfully extracted source are not a file contribution.
			if node.Kind == model.KindFinding {
				continue
			}
			if file.Outcome != Skipped && file.Diagnostic != "symlink" && file.Diagnostic != "special_file" {
				file.Contribution = true
			}
		}
	}
	for i := range l.report.Files {
		f := &l.report.Files[i]
		if f.Outcome == Pending && !f.Contribution {
			f.Outcome = Unprocessed
			f.Diagnostic = "no_contribution"
		}
		if f.Contribution && !hasSuccess(f.Stages) && f.Outcome != FileFailed {
			f.Outcome = Unprocessed
			f.Diagnostic = "not_completed"
			f.Contribution = false
		}
	}
	l.report.Status = Prepared
	l.report.GraphCommit = CommitPending
	return l.snapshot()
}
func hasSuccess(stages []StageDecision) bool {
	for _, s := range stages {
		if s.Outcome == StageSucceeded {
			return true
		}
	}
	return false
}

// Snapshot returns a detached, deterministically ordered report.
func (l *Ledger) Snapshot() Report { l.mu.Lock(); defer l.mu.Unlock(); return l.snapshot() }
func (l *Ledger) snapshot() Report {
	l.report = l.report.recount()
	l.report.Estimate = estimate(l.report.Files, l.report.DiscoveryComplete, l.report.PrunedDirectories)
	l.report.EstimatedTokensSavedOnIngest = nil
	if l.report.Estimate.CoveredFiles > 0 {
		tokens := l.report.Estimate.ObservedMetadataBytes / 4
		l.report.EstimatedTokensSavedOnIngest = &tokens
	}
	r := l.report.Clone()
	slices.SortFunc(r.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return r
}

// Finish applies a valid terminal decision; invalid transitions leave the ledger unchanged.
func (l *Ledger) Finish(status Status, commit GraphCommit) Report {
	l.mu.Lock()
	defer l.mu.Unlock()
	current := l.snapshot()
	result, err := current.Finish(status, commit)
	if err != nil {
		return current
	}
	l.report = result
	return l.snapshot()
}

func textCandidate(file string) bool {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go", ".py", ".js", ".jsx", ".ts", ".tsx", ".vue", ".rs", ".java", ".c", ".h", ".cpp", ".cs", ".rb", ".php", ".md", ".mdx", ".markdown", ".txt", ".yaml", ".yml", ".json", ".toml", ".mod", ".sum", ".sh", ".tpl", ".tmpl", ".gotmpl", ".gohtml":
		return true
	}
	return false
}
func estimate(files []File, complete bool, pruned int) Estimate {
	e := Estimate{Name: "metadata-bytes", Version: 1, BytesPerToken: 4, Baseline: "Encountered excluded text candidates, using file-size metadata only; hypothetical raw context, not model consumption.", Partial: !complete || pruned > 0, Explanation: "No eligible excluded text baseline was observed."}
	for _, f := range files {
		if f.Outcome != Skipped {
			continue
		}
		if f.SkipReason == Binary {
			e.ExcludedBinaryFiles++
			continue
		}
		if !textCandidate(f.Path) || f.SizeBytes == nil || *f.SizeBytes > 10<<20 {
			e.UnknownFiles++
			continue
		}
		e.CoveredFiles++
		e.ObservedMetadataBytes += *f.SizeBytes
	}
	if e.UnknownFiles > 0 {
		e.Partial = true
	}
	if e.CoveredFiles > 0 {
		e.Explanation = "Partial metadata estimate; extension identifies a text candidate, not verified text. Unseen and unknown files are not extrapolated."
	}
	return e
}
