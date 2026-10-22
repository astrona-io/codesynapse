// Package review turns a diff into a review context pack: which symbols
// changed, what they affect, which tests cover them, which docs describe
// them, and how risky each change is. It is deterministic and calls no LLM;
// the Markdown output is meant to be handed to one (or a human) as context.
package review

import (
	"cmp"
	"context"
	"slices"

	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Level is a coarse risk bucket.
type Level string

// Risk levels.
const (
	Low      Level = "low"
	Medium   Level = "medium"
	High     Level = "high"
	Critical Level = "critical"
)

// LevelFor buckets a 0-100 risk score.
func LevelFor(score int) Level {
	switch {
	case score >= 60:
		return Critical
	case score >= 40:
		return High
	case score >= 20:
		return Medium
	default:
		return Low
	}
}

func (l Level) rank() int {
	return slices.Index([]Level{Low, Medium, High, Critical}, l)
}

// Options tunes impact analysis.
type Options struct {
	// Depth bounds caller traversal; default 3.
	Depth int
	// MaxImpacted caps the impacted list in the report; default 50.
	MaxImpacted int
}

// ChangedSymbol is one symbol touched by the diff.
type ChangedSymbol struct {
	query.Ref
	Signature string      `json:"signature,omitempty"`
	Risk      int         `json:"risk"`
	Level     Level       `json:"level"`
	Reasons   []string    `json:"reasons"`
	Tags      []string    `json:"security_tags,omitempty"`
	FanIn     int         `json:"fan_in"`
	Impacted  int         `json:"impacted"`
	Tests     []query.Ref `json:"tests"`
	// Findings are security findings inside the changed symbol.
	Findings []query.Ref `json:"findings,omitempty"`
	// Duplicate is set when the symbol has exact or edited copies: a change
	// to it probably belongs in the copies too, or the copies should be merged.
	Duplicate *duplicates.Info `json:"duplicate,omitempty"`
}

// FileSummary describes one changed file.
type FileSummary struct {
	Path    string `json:"path"`
	Status  string `json:"status"`
	Symbols int    `json:"symbols"`
	// Indexed is false when the graph has no symbols for the file (non-Go
	// files, or a graph built before the change).
	Indexed bool `json:"indexed"`
}

// Summary is the headline of a report.
type Summary struct {
	ChangedFiles      int `json:"changed_files"`
	ChangedSymbols    int `json:"changed_symbols"`
	ImpactedSymbols   int `json:"impacted_symbols"`
	UntestedChanges   int `json:"untested_changes"`
	SecuritySensitive int `json:"security_sensitive"`
	// DuplicatedChanges counts changed functions that have exact or edited copies.
	DuplicatedChanges int   `json:"duplicated_changes"`
	MaxLevel          Level `json:"max_level"`
}

// Report is the full review context pack.
type Report struct {
	Repo      string          `json:"repo"`
	Summary   Summary         `json:"summary"`
	Changes   []ChangedSymbol `json:"changes"`
	Impacted  []query.Ref     `json:"impacted"`
	Files     []FileSummary   `json:"files"`
	Docs      []query.Ref     `json:"docs,omitempty"`
	Checklist []string        `json:"checklist,omitempty"`
}

// Analyze builds a Report for changes against the graph of repo. The graph
// should reflect the head side of the diff.
func Analyze(ctx context.Context, svc query.Service, repo string, changes []gitx.FileChange, opts Options) (Report, error) {
	if opts.Depth <= 0 {
		opts.Depth = 3
	}
	if opts.MaxImpacted <= 0 {
		opts.MaxImpacted = 50
	}
	rep := Report{Repo: repo, Summary: Summary{MaxLevel: Low}}
	changedIDs := map[string]bool{}
	var changed []model.Node

	for _, fc := range changes {
		fs := FileSummary{Path: fc.Path, Status: status(fc)}
		if !fc.Deleted {
			seen := map[string]bool{}
			for _, r := range fc.Ranges {
				syms, err := svc.SymbolsAt(ctx, repo, fc.Path, r.Start, r.End)
				if err != nil {
					return rep, err
				}
				for _, s := range syms {
					// A type's range covers its fields only; methods are separate
					// nodes, so keeping every overlapping symbol is correct.
					if seen[s.ID] {
						continue
					}
					seen[s.ID] = true
					fs.Symbols++
					if !changedIDs[s.ID] {
						changedIDs[s.ID] = true
						changed = append(changed, s)
					}
				}
			}
			fs.Indexed = fs.Symbols > 0
		}
		rep.Files = append(rep.Files, fs)
	}

	dupReport, haveDups := query.DuplicateReport(ctx, svc.R, repo)
	impacted := map[string]query.Ref{}
	docs := map[string]query.Ref{}
	tags := map[string]bool{}
	for _, n := range changed {
		cs, hits, err := analyzeSymbol(ctx, svc, n, opts.Depth)
		if err != nil {
			return rep, err
		}
		for _, h := range hits {
			if changedIDs[h.Node.ID] {
				continue
			}
			if prev, ok := impacted[h.Node.ID]; !ok || h.Depth < prev.Depth {
				r := query.RefOf(h.Node)
				r.Depth = h.Depth
				impacted[h.Node.ID] = r
			}
		}
		mentions, err := svc.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeMentions}, store.In, 1, 20)
		if err != nil {
			return rep, err
		}
		for _, m := range mentions {
			docs[m.Node.ID] = query.RefOf(m.Node)
		}
		for _, t := range cs.Tags {
			tags[t] = true
		}
		if len(cs.Tests) == 0 && !n.IsTest {
			rep.Summary.UntestedChanges++
		}
		if len(cs.Tags) > 0 {
			rep.Summary.SecuritySensitive++
		}
		if haveDups && n.Kind.IsCallable() && !n.IsTest {
			if cs.Duplicate = dupReport.Lookup(n.ID); cs.Duplicate != nil {
				rep.Summary.DuplicatedChanges++
			}
		}
		if cs.Level.rank() > rep.Summary.MaxLevel.rank() {
			rep.Summary.MaxLevel = cs.Level
		}
		rep.Changes = append(rep.Changes, cs)
	}

	slices.SortStableFunc(rep.Changes, func(a, b ChangedSymbol) int {
		return cmp.Or(cmp.Compare(b.Risk, a.Risk), cmp.Compare(a.Name, b.Name))
	})
	for _, r := range impacted {
		rep.Impacted = append(rep.Impacted, r)
	}
	slices.SortFunc(rep.Impacted, func(a, b query.Ref) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Name, b.Name))
	})
	rep.Summary.ImpactedSymbols = len(rep.Impacted)
	rep.Impacted = rep.Impacted[:min(len(rep.Impacted), opts.MaxImpacted)]
	for _, d := range docs {
		rep.Docs = append(rep.Docs, d)
	}
	slices.SortFunc(rep.Docs, func(a, b query.Ref) int { return cmp.Compare(a.Location, b.Location) })

	rep.Summary.ChangedFiles = len(rep.Files)
	rep.Summary.ChangedSymbols = len(rep.Changes)
	rep.Checklist = checklist(tags, rep.Summary.UntestedChanges > 0, len(rep.Docs) > 0, rep.Summary.DuplicatedChanges > 0)
	return rep, nil
}

func status(fc gitx.FileChange) string {
	switch {
	case fc.Added:
		return "added"
	case fc.Deleted:
		return "deleted"
	case fc.OldPath != "":
		return "renamed"
	default:
		return "modified"
	}
}

func analyzeSymbol(ctx context.Context, svc query.Service, n model.Node, depth int) (ChangedSymbol, []model.Hit, error) {
	cs := ChangedSymbol{Ref: query.RefOf(n), Signature: n.Signature, Tags: n.SecurityTags}
	if n.Kind == model.KindType {
		// Types have no callers of their own; consider them low-impact unless
		// they carry other signals.
		cs.Risk, cs.Reasons = Score(Signals{Exported: n.Exported, Complexity: n.Complexity, Tested: true})
		cs.Level = LevelFor(cs.Risk)
		return cs, nil, nil
	}
	hits, err := svc.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}, store.In, depth, store.MaxLimit)
	if err != nil {
		return cs, nil, err
	}
	for _, h := range hits {
		if h.Depth == 1 {
			cs.FanIn++
		}
		if h.Node.IsTest {
			r := query.RefOf(h.Node)
			r.Depth = h.Depth
			cs.Tests = append(cs.Tests, r)
		}
	}
	cs.Impacted = len(hits) - len(cs.Tests)
	findings, err := svc.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeHasFinding}, store.Out, 1, 20)
	if err != nil {
		return cs, nil, err
	}
	var sevs []string
	for _, f := range findings {
		sevs = append(sevs, f.Node.Severity)
		cs.Findings = append(cs.Findings, query.RefOf(f.Node))
	}
	cs.Risk, cs.Reasons = Score(Signals{
		FanIn: cs.FanIn, Impacted: cs.Impacted, Exported: n.Exported, Complexity: n.Complexity,
		Tags: n.SecurityTags, Tested: len(cs.Tests) > 0 || n.IsTest, Findings: sevs,
	})
	cs.Level = LevelFor(cs.Risk)
	return cs, hits, nil
}

// Signals are the inputs to Score.
type Signals struct {
	FanIn      int
	Impacted   int
	Exported   bool
	Complexity int
	Tags       []string
	Tested     bool
	// Findings are the severities of security findings in the symbol.
	Findings []string
}

// highRiskTags weigh more than other security tags.
var highRiskTags = map[string]bool{
	"exec": true, "sql": true, "cypher": true, "unsafe": true, "deserialize": true,
	"auth": true, "weak-crypto": true, "syscall": true, "template": true,
}

// Score computes a 0-100 risk score and the reasons that contributed.
func Score(s Signals) (int, []string) {
	score := 0
	var reasons []string
	if s.FanIn > 0 {
		score += min(s.FanIn*3, 25)
		reasons = append(reasons, plural(s.FanIn, "direct caller"))
	}
	if s.Impacted > s.FanIn {
		score += min(s.Impacted, 20)
		reasons = append(reasons, plural(s.Impacted, "transitively impacted symbol"))
	}
	if s.Exported {
		score += 10
		reasons = append(reasons, "exported API")
	}
	switch {
	case s.Complexity > 15:
		score += 15
		reasons = append(reasons, "very high complexity")
	case s.Complexity > 10:
		score += 10
		reasons = append(reasons, "high complexity")
	case s.Complexity > 5:
		score += 5
		reasons = append(reasons, "moderate complexity")
	}
	tagScore := 0
	for _, t := range s.Tags {
		if highRiskTags[t] {
			tagScore += 15
		} else {
			tagScore += 8
		}
		reasons = append(reasons, "touches "+t)
	}
	score += min(tagScore, 30)
	if !s.Tested {
		score += 15
		reasons = append(reasons, "no test reaches it")
	}
	findingScore := 0
	for _, sev := range s.Findings {
		findingScore += map[string]int{"critical": 30, "high": 20, "medium": 10, "low": 3}[sev]
		reasons = append(reasons, sev+" security finding")
	}
	score += min(findingScore, 40)
	return min(score, 100), reasons
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return itoa(n) + " " + noun + "s"
}

var tagChecks = map[string]string{
	"exec":          "Command execution: arguments come from trusted input, no shell, explicit argv, context timeout.",
	"sql":           "SQL: all values parameterized; no string-built queries; rows/statements closed.",
	"cypher":        "Cypher: values passed as parameters; labels/types only from allowlists.",
	"http":          "HTTP: timeouts set, bodies size-limited and closed, redirects and TLS verified.",
	"network":       "Network: dial/listen timeouts, addresses validated, no SSRF to internal ranges.",
	"filesystem":    "Filesystem: paths cleaned and confined to the intended root; symlinks considered; permissions tight.",
	"crypto":        "Crypto: standard primitives, crypto/rand for secrets, constant-time comparisons.",
	"weak-crypto":   "Weak crypto (MD5/SHA1/DES/RC4): only for non-security checksums; replace otherwise.",
	"insecure-rand": "math/rand: never for tokens, keys or IDs that must be unguessable.",
	"tls":           "TLS: InsecureSkipVerify not set; MinVersion >= TLS 1.2.",
	"unsafe":        "unsafe: justify each use; check alignment and lifetime assumptions.",
	"reflect":       "reflect: check for panics on unexpected kinds; avoid on hot paths.",
	"deserialize":   "Deserialization: size limits, unknown fields handled, untrusted input validated after decode.",
	"template":      "Templates: html/template for HTML; no template.HTML from untrusted data.",
	"env":           "Environment: secrets not logged; defaults safe when unset.",
	"syscall":       "Syscalls: error handling and privilege assumptions reviewed.",
	"auth":          "AuthN/Z: tokens validated (issuer, audience, expiry, algorithm); authorization checked server-side.",
}

func checklist(tags map[string]bool, untested, docs, duplicated bool) []string {
	var out []string
	keys := make([]string, 0, len(tags))
	for t := range tags {
		keys = append(keys, t)
	}
	slices.Sort(keys)
	for _, t := range keys {
		if c, ok := tagChecks[t]; ok {
			out = append(out, c)
		}
	}
	if untested {
		out = append(out, "Add or extend tests for changed symbols that no test reaches.")
	}
	if duplicated {
		out = append(out, "Changed functions with copies: apply the same change to each copy, or merge the copies into one so the next change is made once.")
	}
	if docs {
		out = append(out, "Check that the listed documentation sections still match the changed behaviour.")
	}
	return out
}
