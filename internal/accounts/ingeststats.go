package accounts

import "time"

// Ingest figures of the platform statistics (docs/plans/PLATFORM_STATS_PLAN.md,
// phase 4): counts summed from the ingest runs of a window. They carry an
// organisation ID and numbers only, never a repository, graph, run ID, path
// or report text, so they may cross tenants.

// IngestSkips are skipped files by reason.
type IngestSkips struct {
	IgnoredPath   int `json:"ignored_path"`
	GeneratedCode int `json:"generated_code"`
	Binary        int `json:"binary"`
	NonK8sYAML    int `json:"non_k8s_yaml"`
}

// IngestFigures are the runs and files of a window. A finished run counts in
// the window it finished in (its start when its report expired). Running
// counts the runs not finished yet that started before the window ended.
// WithoutReport counts finished runs whose report has expired
// (IngestHistory): their status is known, their file counts are not, so the
// file figures cover the other runs only.
type IngestFigures struct {
	Succeeded           int         `json:"succeeded"`
	Failed              int         `json:"failed"`
	Cancelled           int         `json:"cancelled"`
	Running             int         `json:"running"`
	WithoutReport       int         `json:"without_report"`
	IncompleteDiscovery int         `json:"incomplete_discovery"`
	Scanned             int         `json:"scanned"`
	Indexed             int         `json:"indexed"`
	Skipped             int         `json:"skipped"`
	FilesFailed         int         `json:"files_failed"`
	Unprocessed         int         `json:"unprocessed"`
	Skips               IngestSkips `json:"skips"`
}

// Add adds o to f.
func (f *IngestFigures) Add(o IngestFigures) {
	f.Succeeded += o.Succeeded
	f.Failed += o.Failed
	f.Cancelled += o.Cancelled
	f.Running += o.Running
	f.WithoutReport += o.WithoutReport
	f.IncompleteDiscovery += o.IncompleteDiscovery
	f.Scanned += o.Scanned
	f.Indexed += o.Indexed
	f.Skipped += o.Skipped
	f.FilesFailed += o.FilesFailed
	f.Unprocessed += o.Unprocessed
	f.Skips.IgnoredPath += o.Skips.IgnoredPath
	f.Skips.GeneratedCode += o.Skips.GeneratedCode
	f.Skips.Binary += o.Skips.Binary
	f.Skips.NonK8sYAML += o.Skips.NonK8sYAML
}

// OrgIngestStats are one organisation's figures. OrgExists is false for an
// organisation deleted since; its runs still count in the platform total.
type OrgIngestStats struct {
	OrgID     string
	OrgExists bool
	IngestFigures
}

// statCount is one dimension's count.
type statCount struct {
	dim string
	n   int
}

// ingestStatRows are the rollup dimensions of a day's figures.
func ingestStatRows(f IngestFigures) map[string][]statCount {
	return map[string][]statCount{
		StatIngestRuns: {{"succeeded", f.Succeeded}, {"failed", f.Failed}, {"cancelled", f.Cancelled},
			{"incomplete_discovery", f.IncompleteDiscovery}, {"without_report", f.WithoutReport}},
		StatIngestFiles: {{"scanned", f.Scanned}, {"indexed", f.Indexed}, {"skipped", f.Skipped}, {"failed", f.FilesFailed}, {"unprocessed", f.Unprocessed}},
		StatIngestSkips: {{"ignored_path", f.Skips.IgnoredPath}, {"generated_code", f.Skips.GeneratedCode}, {"binary", f.Skips.Binary}, {"non_k8s_yaml", f.Skips.NonK8sYAML}},
	}
}

// IngestFiguresOf reads a day's ingest figures back from summed rollup rows
// (metric, dimension → value). Running is not kept.
func IngestFiguresOf(get func(metric, dim string) int) IngestFigures {
	return IngestFigures{
		Succeeded: get(StatIngestRuns, "succeeded"), Failed: get(StatIngestRuns, "failed"), Cancelled: get(StatIngestRuns, "cancelled"),
		IncompleteDiscovery: get(StatIngestRuns, "incomplete_discovery"), WithoutReport: get(StatIngestRuns, "without_report"),
		Scanned: get(StatIngestFiles, "scanned"), Indexed: get(StatIngestFiles, "indexed"), Skipped: get(StatIngestFiles, "skipped"),
		FilesFailed: get(StatIngestFiles, "failed"), Unprocessed: get(StatIngestFiles, "unprocessed"),
		Skips: IngestSkips{IgnoredPath: get(StatIngestSkips, "ignored_path"), GeneratedCode: get(StatIngestSkips, "generated_code"),
			Binary: get(StatIngestSkips, "binary"), NonK8sYAML: get(StatIngestSkips, "non_k8s_yaml")},
	}
}

// AddIngestStats adds a day's ingest figures to the rollup: every run to the
// platform totals, and to the organisation's rows while it exists. Running
// runs are not kept: a day's figure is what finished that day.
func AddIngestStats(set *PlatformStatSet, rows []OrgIngestStats) {
	for _, r := range rows {
		for metric, counts := range ingestStatRows(r.IngestFigures) {
			for _, c := range counts {
				set.Total(metric, c.dim, int64(c.n))
				if r.OrgExists {
					set.Org(r.OrgID, metric, c.dim, int64(c.n))
				}
			}
		}
	}
}

// IngestInWindow reports whether a run counts in [from, to): a finished run
// by when it finished (its start when that is unknown), a running one when
// it started before to.
func IngestInWindow(terminal bool, started time.Time, finished *time.Time, from, to time.Time) bool {
	if !terminal {
		return started.Before(to)
	}
	at := started
	if finished != nil {
		at = *finished
	}
	return !at.Before(from) && at.Before(to)
}
