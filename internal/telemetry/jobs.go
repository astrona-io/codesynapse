package telemetry

import (
	"context"
	"time"
)

// Job outcomes, the fixed values of codesynapse_jobs_finished_total's
// outcome label.
const (
	JobOK         = "ok"
	JobError      = "error"
	JobLeftBehind = "left_behind" // left for another process to resume
	JobCancelled  = "cancelled"
)

// jobKinds are the job kinds the API and workers run (services/api jobs.go
// and jobs_durable.go); anything else is recorded as "other".
var jobKinds = map[string]bool{"ingest": true, "sync": true, "clone": true, "switch": true, "ai": true, "ai-auto": true, "fix": true}

var jobOutcomes = map[string]bool{JobOK: true, JobError: true, JobLeftBehind: true, JobCancelled: true}

// JobKind is the kind label for a job kind.
func JobKind(kind string) string {
	if jobKinds[kind] {
		return kind
	}
	return "other"
}

// RecordJobFinished counts a job that ended. Unknown outcomes are ignored.
func RecordJobFinished(ctx context.Context, kind, outcome string) {
	if !jobOutcomes[outcome] {
		return
	}
	JobsFinished.IncContext(ctx, JobKind(kind), outcome)
}

// RecordJobWait records how long a job waited before a process claimed it.
func RecordJobWait(ctx context.Context, kind string, d time.Duration) {
	JobWait.ObserveContext(ctx, max(0, d.Seconds()), JobKind(kind))
}

// RecordJobDuration records how long a claimed job ran.
func RecordJobDuration(ctx context.Context, kind string, d time.Duration) {
	JobDuration.ObserveContext(ctx, max(0, d.Seconds()), JobKind(kind))
}

// Phases of an indexing job, as its trace spans name them (job.<phase>).
const (
	PhasePrepare = "prepare" // clone, or fetch and checkout
	PhaseIngest  = "ingest"  // extraction and analysis
	PhasePublish = "publish" // storing the graph
)

var ingestPhases = map[string]bool{PhasePrepare: true, PhaseIngest: true, PhasePublish: true}

// RecordIngestPhase adds one run of an indexing phase. Unknown phases are
// ignored.
func RecordIngestPhase(ctx context.Context, phase string, d time.Duration, ok bool) {
	if !ingestPhases[phase] {
		return
	}
	outcome := "ok"
	if !ok {
		outcome = "error"
	}
	IngestPhaseSeconds.AddContext(ctx, max(0, d.Seconds()), phase, outcome)
	IngestPhaseRuns.IncContext(ctx, phase, outcome)
}

// extractLanguages maps extractor names (ingest.Options.Extracted) to one
// label per parser: JavaScript, TypeScript and Vue share one, as do Java
// and Kotlin.
var extractLanguages = map[string]string{
	"go": "go", "python": "python", "javascript": "jsts", "typescript": "jsts", "vue": "jsts",
	"java": "jvm", "kotlin": "jvm", "rust": "rust", "csharp": "csharp", "php": "php", "ruby": "ruby",
	"docs": "docs", "config": "config",
}

// ExtractLanguage is the language label for an extractor name; anything
// else is "other".
func ExtractLanguage(extractor string) string {
	if l, ok := extractLanguages[extractor]; ok {
		return l
	}
	return "other"
}

// RecordExtract adds the time one extractor took.
func RecordExtract(ctx context.Context, extractor string, d time.Duration) {
	IngestExtract.AddContext(ctx, max(0, d.Seconds()), ExtractLanguage(extractor))
}
