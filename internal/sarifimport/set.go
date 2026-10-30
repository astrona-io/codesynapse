package sarifimport

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"
)

// MaxStored bounds the findings kept for one namespace across its tools.
const MaxStored = 50_000

// Set is what a namespace keeps: the latest upload of each tool.
type Set struct {
	Runs []StoredRun `json:"runs"`
}

// StoredRun is one tool's latest upload.
type StoredRun struct {
	Run
	// Commit is the commit the tool scanned, as the uploader said.
	Commit     string    `json:"commit"`
	UploadedAt time.Time `json:"uploaded_at"`
	// Unmatched counts findings at absolute paths no indexed file matched.
	Unmatched int `json:"unmatched,omitempty"`
}

// Total is the number of findings across tools.
func (s Set) Total() int {
	n := 0
	for _, r := range s.Runs {
		n += len(r.Findings)
	}
	return n
}

// With returns s with each run in up replacing the earlier upload of the
// same tool (case-insensitive); other tools are kept. Runs stay sorted by
// tool.
func (s Set) With(up []StoredRun) Set {
	out := Set{Runs: slices.Clone(s.Runs)}
	for _, r := range up {
		out.Runs = slices.DeleteFunc(out.Runs, func(o StoredRun) bool { return strings.EqualFold(o.Tool, r.Tool) })
		out.Runs = append(out.Runs, r)
	}
	slices.SortFunc(out.Runs, func(a, b StoredRun) int { return cmp.Compare(strings.ToLower(a.Tool), strings.ToLower(b.Tool)) })
	return out
}

// Without returns s without the tool's run, and whether it had one.
func (s Set) Without(tool string) (Set, bool) {
	out := Set{Runs: slices.Clone(s.Runs)}
	n := len(out.Runs)
	out.Runs = slices.DeleteFunc(out.Runs, func(o StoredRun) bool { return strings.EqualFold(o.Tool, tool) })
	return out, len(out.Runs) != n
}

// Files says whether a repository-relative path is an indexed file.
type Files interface {
	HasFile(ctx context.Context, file string) (bool, error)
}

// maxSuffixTries bounds the lookups for one absolute path: its last
// maxSuffixTries segments at most. maxSearches bounds the paths searched
// that way in one run; the rest only try the roots learned so far.
const (
	maxSuffixTries = 32
	maxSearches    = 2000
)

// Place resolves findings at absolute paths the log's bases did not cover
// to the longest suffix that is an indexed file (a CI runner's checkout
// path is unknown here), drops those it cannot place and returns how many
// it dropped. Each path is looked up once, and the checkout root a match
// reveals is tried first for the next path.
func Place(ctx context.Context, files Files, run *Run) (unmatched int, err error) {
	p := placer{files: files, cache: map[string]string{}}
	kept := run.Findings[:0]
	for _, f := range run.Findings {
		if f.abs == "" {
			kept = append(kept, f)
			continue
		}
		rel, err := p.place(ctx, f.abs)
		if err != nil {
			return 0, err
		}
		if rel == "" {
			unmatched++
			continue
		}
		f.File, f.abs = rel, ""
		kept = append(kept, f)
	}
	run.Findings = kept
	return unmatched, nil
}

type placer struct {
	files    Files
	cache    map[string]string
	roots    []string
	searches int
}

func (p *placer) place(ctx context.Context, abs string) (string, error) {
	if rel, ok := p.cache[abs]; ok {
		return rel, nil
	}
	rel, err := p.lookup(ctx, abs)
	if err != nil {
		return "", err
	}
	p.cache[abs] = rel
	return rel, nil
}

func (p *placer) lookup(ctx context.Context, abs string) (string, error) {
	for _, root := range p.roots {
		if rest, ok := strings.CutPrefix(abs, root); ok {
			if rel, ok := CleanPath(rest); ok {
				has, err := p.files.HasFile(ctx, rel)
				if err != nil || has {
					return rel, err
				}
			}
		}
	}
	if p.searches >= maxSearches {
		return "", nil
	}
	p.searches++
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	for i := max(0, len(parts)-maxSuffixTries); i < len(parts); i++ {
		rel, ok := CleanPath(strings.Join(parts[i:], "/"))
		if !ok {
			continue
		}
		has, err := p.files.HasFile(ctx, rel)
		if err != nil {
			return "", err
		}
		if has {
			if root := strings.TrimSuffix(abs, rel); len(p.roots) < MaxRuns && !slices.Contains(p.roots, root) {
				p.roots = append(p.roots, root)
			}
			return rel, nil
		}
	}
	return "", nil
}
