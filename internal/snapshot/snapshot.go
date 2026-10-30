// Package snapshot takes a history.Snapshot of a repository from the reports
// its last ingest stored, and records it after each ingest.
package snapshot

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/cleanup"
	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/metrics"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/rules"
)

// Take reads the stored reports of repo. A report an older ingest never
// stored counts as zeros, which a later snapshot then corrects.
func Take(ctx context.Context, r store.Reader, repo string) (history.Snapshot, error) {
	n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return history.Snapshot{}, err
	}
	s := history.Snapshot{At: n.Time, Head: n.Head, Branch: n.Branch}

	sec, err := security.Build(ctx, r, repo, security.Options{MinSeverity: "info"})
	if err != nil {
		return s, fmt.Errorf("security report: %w", err)
	}
	s.Grade, s.Score = sec.Grade, sec.Score
	s.Findings, s.Vulns = sec.FindingCounts, sec.VulnCounts
	for _, v := range sec.Vulnerabilities {
		if v.Reachability == "called" {
			s.VulnsCalled++
		}
	}
	for _, rc := range sec.Rules {
		if _, ok := rules.Get(rc.Rule); ok && rc.Count > 0 {
			if s.Rules == nil {
				s.Rules = map[string]int{}
			}
			s.Rules[rc.Rule] += rc.Count
		}
	}
	s.Modules, s.Direct = sec.Modules.Total, sec.Modules.Direct

	st, err := r.Stats(ctx, repo)
	if err != nil {
		return s, fmt.Errorf("graph stats: %w", err)
	}
	for f, n := range st.Formats {
		if n <= 0 {
			continue
		}
		if !Formats[f] {
			f = "other"
		}
		if s.Formats == nil {
			s.Formats = map[string]int{}
		}
		s.Formats[f] += n
	}

	if n.Cleanup != "" {
		var c cleanup.Report
		if err := json.Unmarshal([]byte(n.Cleanup), &c); err != nil {
			return s, fmt.Errorf("decoding cleanup report: %w", err)
		}
		s.Functions = c.Summary.Functions
		s.DeadFunctions, s.DeadLOC, s.UnusedModules = c.Summary.Unreachable+c.Summary.TestOnly, c.Summary.DeadLOC, c.Summary.UnusedModules
	}
	if n.Duplicates != "" {
		d, err := duplicates.Decode(n.Duplicates)
		if err != nil {
			return s, err
		}
		s.FunctionLOC, s.DupLOC, s.NearLOC, s.DupPct = d.Summary.FunctionLOC, d.Summary.RedundantLOC, d.Summary.NearRedundantLOC, d.Summary.Percent
	}
	if n.Licenses != "" {
		var l license.Report
		if err := json.Unmarshal([]byte(n.Licenses), &l); err != nil {
			return s, fmt.Errorf("decoding licence report: %w", err)
		}
		s.LicenceConflicts, s.LicenceReview = l.Conflict, l.Review
	}
	if n.Metrics != "" {
		var m metrics.Report
		if err := json.Unmarshal([]byte(n.Metrics), &m); err != nil {
			return s, fmt.Errorf("decoding metrics: %w", err)
		}
		s.TestReachPct, s.DocCoveragePct = m.Tests.ReachablePct, m.Docs.CoveragePct
	}
	return s, nil
}

// Formats are the languages and formats a snapshot names (model.FileFormat
// for the parsers that exist). They feed cross-tenant counts, so any other
// name, which a future parser or a damaged graph could bring, counts as
// "other" rather than reaching the platform statistics.
var Formats = map[string]bool{
	"go": true, "python": true, "javascript": true, "typescript": true, "vue": true, "java": true, "kotlin": true,
	"rust": true, "csharp": true, "php": true, "ruby": true, "yaml": true, "helm": true, "go-template": true, "markdown": true,
}

// Store is what Record needs: reports to read and a place to keep the snapshot.
type Store interface {
	store.Reader
	store.HistoryStore
}

// Record takes a snapshot of repo and stores it. A failure is logged and
// dropped: history is an addition, and an ingest that worked must not fail
// because of it.
func Record(ctx context.Context, st Store, repo string) {
	logger := logx.FromContext(ctx)
	s, err := Take(ctx, st, repo)
	if err != nil {
		logger.Warn("taking a history snapshot failed", "repo", repo, logx.Error(err))
		return
	}
	if err := st.AddSnapshot(ctx, repo, s); err != nil {
		logger.Warn("saving a history snapshot failed", "repo", repo, logx.Error(err))
	}
}
