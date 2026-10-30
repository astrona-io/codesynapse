package memstore

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
)

// IngestStats implements accounts.Store, with the rules of the PostgreSQL
// query.
func (s *Store) IngestStats(_ context.Context, from, to time.Time) ([]accounts.OrgIngestStats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ingestStats(from, to), nil
}

// ingestStats needs s.mu held. A run whose report expired counts by its
// start, as PostgreSQL no longer has its finish.
func (s *Store) ingestStats(from, to time.Time) []accounts.OrgIngestStats {
	per := map[string]*accounts.OrgIngestStats{}
	for _, r := range s.ingestRuns {
		rep := r.run.Report
		finished := rep.FinishedAt
		if !r.retained {
			finished = nil
		}
		if !accounts.IngestInWindow(r.terminal, rep.StartedAt, finished, from, to) {
			continue
		}
		o := per[r.run.OrgID]
		if o == nil {
			_, exists := s.orgs[r.run.OrgID]
			o = &accounts.OrgIngestStats{OrgID: r.run.OrgID, OrgExists: exists}
			per[r.run.OrgID] = o
		}
		f := &o.IngestFigures
		if !r.terminal {
			f.Running++
			continue
		}
		switch rep.Status {
		case ingestreport.Succeeded:
			f.Succeeded++
		case ingestreport.Failed:
			f.Failed++
		case ingestreport.Cancelled:
			f.Cancelled++
		}
		if !r.retained {
			f.WithoutReport++
			continue
		}
		if !rep.DiscoveryComplete {
			f.IncompleteDiscovery++
		}
		f.Scanned += rep.TotalFilesScanned
		f.Indexed += rep.FilesIndexed
		f.Skipped += rep.FilesSkipped
		f.FilesFailed += rep.FilesFailed
		f.Unprocessed += rep.FilesUnprocessed
		f.Skips.IgnoredPath += rep.SkipBreakdown[ingestreport.IgnoredPath]
		f.Skips.GeneratedCode += rep.SkipBreakdown[ingestreport.GeneratedCode]
		f.Skips.Binary += rep.SkipBreakdown[ingestreport.Binary]
		f.Skips.NonK8sYAML += rep.SkipBreakdown[ingestreport.NonKubernetesYAML]
	}
	out := make([]accounts.OrgIngestStats, 0, len(per))
	for _, o := range per {
		out = append(out, *o)
	}
	slices.SortFunc(out, func(a, b accounts.OrgIngestStats) int { return strings.Compare(a.OrgID, b.OrgID) })
	return out
}
