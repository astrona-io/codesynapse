package api

import "maps"

// Cross-tenant charts of the jobs and ingest sections
// (docs/PLATFORM_STATS_PLAN.md, phase 4). Job kinds, outcomes, phases and
// skip reasons are fixed lists the code sets, never tenant data, so they are
// shown whole; languages count only where at least minGroupOrgs
// organisations indexed them. The file counters carry a repository label
// (an opaque entry ID); every query sums it away, and TestAdminPanelsNoRepo
// keeps it that way. Series of work without a tenant export as platform_
// twins, so the regular expressions take both.
var jobPanels = map[string]adminPanel{
	"jobs_finished": {seriesPanel: seriesPanel{Title: "Finished jobs", Unit: "jobs", Kind: "count",
		Help:    "Indexing, sync, AI and fix jobs that ended, by outcome. Left behind means another process resumes the job.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase({__name__=~"(platform_)?codesynapse_jobs_finished_total"}[$w]))`, by: "outcome"}}}},
	"jobs_org_finished": {byTenant: true, seriesPanel: seriesPanel{Title: "Finished jobs by organisation", Unit: "jobs", Kind: "count",
		Help:    "Jobs that ended per step, by organisation. Jobs without an organisation are not shown here.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(codesynapse_jobs_finished_total[$w]))`, by: "tenant_id"}}}},
	"jobs_failures": {seriesPanel: seriesPanel{Title: "Failed jobs", Unit: "jobs", Kind: "count",
		Help:    "Jobs that ended in an error, by kind. Cancelled jobs and jobs left for another process are not failures.",
		Empty:   "No jobs failed in this window.",
		queries: []seriesQuery{{expr: `sum by (kind) (increase({__name__=~"(platform_)?codesynapse_jobs_finished_total",outcome="error"}[$w]))`, by: "kind"}}}},
	"jobs_wait": {seriesPanel: seriesPanel{Title: "Job queue wait", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time from queueing a job to a process starting it, by kind, across organisations.",
		Empty:   "No jobs started in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, kind) (increase({__name__=~"(platform_)?codesynapse_job_wait_seconds_bucket"}[$w])))`, by: "kind"}}}},
	"jobs_duration": {seriesPanel: seriesPanel{Title: "Job run time", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time from a process starting a job to its end, by kind, across organisations.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, kind) (increase({__name__=~"(platform_)?codesynapse_job_duration_seconds_bucket"}[$w])))`, by: "kind"}}}},
	"jobs_queue": {seriesPanel: seriesPanel{Title: "Job queue", Unit: "jobs", Kind: "gauge",
		Help:    "Jobs a worker could claim, by kind, reported by the process running the scheduler.",
		Empty:   "Nothing waited in this window.",
		queries: []seriesQuery{{expr: `max by (kind) (platform_codesynapse_jobs_waiting)`, by: "kind"}}}},
	"jobs_leases": {seriesPanel: seriesPanel{Title: "Lease changes", Unit: "events", Kind: "count",
		Help:    "Scheduler lease taken, lost or not renewed, and durable job leases not renewed or given up.",
		Empty:   "No lease changed in this window.",
		queries: []seriesQuery{{expr: `sum by (series) (label_join(increase(platform_codesynapse_lease_events_total[$w]), "series", " ", "lease", "event"))`, by: "series"}}}},
	"jobs_scheduler": {seriesPanel: seriesPanel{Title: "Scheduler holders", Unit: "processes", Kind: "gauge",
		Help:    "API processes that hold the scheduler lease. Anything but 1 means no process, or more than one, runs the scheduler and maintenance loops.",
		queries: []seriesQuery{{name: "holders", expr: `sum(platform_codesynapse_loops_lease_held)`}}}},
	"jobs_worker_roles": {seriesPanel: seriesPanel{Title: "Workers by role", Unit: "processes", Kind: "gauge",
		Help:    "Processes that run jobs, by role: ingest (clone, sync, index), ai and fix. A process with several roles counts under each.",
		Empty:   "No worker reported in this window.",
		queries: []seriesQuery{{expr: `sum by (role) (platform_codesynapse_worker_roles)`, by: "role"}}}},
	"ingest_runs": {seriesPanel: seriesPanel{Title: "Indexing runs", Unit: "runs", Kind: "count",
		Help:    "Repository indexing runs by outcome, across organisations.",
		Empty:   "No repository was indexed in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase({__name__=~"(platform_)?ingest_runs_total"}[$w]))`, by: "outcome"}}}},
	"ingest_org_runs": {byTenant: true, seriesPanel: seriesPanel{Title: "Indexing runs by organisation", Unit: "runs", Kind: "count",
		Help:    "Repository indexing runs per step, by organisation.",
		Empty:   "No repository was indexed in this window.",
		queries: []seriesQuery{{expr: `sum by (tenant_id) (increase(ingest_runs_total[$w]))`, by: "tenant_id"}}}},
	"ingest_phases": {seriesPanel: seriesPanel{Title: "Indexing phase time", Unit: "seconds", Kind: "gauge",
		Help:    "Average time per run of each indexing phase: prepare (clone or fetch), ingest (extraction and analysis) and publish (storing the graph).",
		Empty:   "No indexing phase ran in this window.",
		queries: []seriesQuery{{expr: `sum by (phase) (increase({__name__=~"(platform_)?codesynapse_ingest_phase_seconds_total"}[$w])) / (sum by (phase) (increase({__name__=~"(platform_)?codesynapse_ingest_phase_total"}[$w])) > 0)`, by: "phase"}}}},
	"ingest_extract": {groups: `count by (language) (sum by (language, tenant_id) (increase(codesynapse_ingest_extract_seconds_total[$w])) > 0)`,
		seriesPanel: seriesPanel{Title: "Extraction time by language", Unit: "seconds", Kind: "count",
			Help:    "Time spent parsing each language while indexing. A language fewer than three organisations indexed in the range is counted under other.",
			Empty:   "No extraction ran in this window.",
			queries: []seriesQuery{{expr: `sum by (language) (increase(codesynapse_ingest_extract_seconds_total[$w]))`, by: "language"}}}},
	"ingest_files": {seriesPanel: seriesPanel{Title: "Files across runs", Unit: "files", Kind: "count",
		Help:    "Files encountered and confirmed file writes, summed over every repository and organisation.",
		Empty:   "No file was processed in this window.",
		queries: []seriesQuery{{name: "scanned", expr: `sum(increase(codesynapse_files_scanned_total[$w]))`}, {name: "written", expr: `sum(increase(codesynapse_files_indexed_total[$w]))`}}}},
	"ingest_skips": {seriesPanel: seriesPanel{Title: "Skipped files", Unit: "files", Kind: "count",
		Help:    "Whole-file skip decisions by reason, summed over every repository and organisation.",
		Empty:   "No file was skipped in this window.",
		queries: []seriesQuery{{expr: `sum by (skip_reason) (increase(codesynapse_files_skipped_total[$w]))`, by: "skip_reason"}}}},
}

func init() { maps.Copy(adminPanels, jobPanels) }
