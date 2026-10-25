package gittools

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/gitx"
)

// DiffIn is git_diff_summary's input.
type DiffIn struct {
	// Base is a branch, tag or commit to compare against: the change since
	// the point where the current branch left it (the merge base), working
	// tree included. Empty compares with HEAD: everything uncommitted.
	Base string `json:"base,omitempty" jsonschema:"branch, tag or commit to compare against (the change since HEAD left it, working tree included); default HEAD: everything uncommitted"`
	// Paths limits the answer to these repository-relative paths.
	Paths []string `json:"paths,omitempty" jsonschema:"limit to these repo-relative paths"`
}

// Hunk is one changed region. New and Old are line ranges ("12-18"); In is
// the enclosing declaration git shows in the hunk heading, when it knows one.
type Hunk struct {
	New string `json:"new"`
	Old string `json:"old,omitempty"`
	In  string `json:"in,omitempty"`
}

// DiffFile is one changed path with where it changed.
type DiffFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // M, A, D or ? (untracked)
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Binary  bool   `json:"binary,omitempty"`
	Hunks   []Hunk `json:"hunks,omitempty"`
	// HunksOmitted counts hunks beyond MaxHunks.
	HunksOmitted int `json:"hunks_omitted,omitempty"`
}

// DiffOut is git_diff_summary's answer.
type DiffOut struct {
	Against   string     `json:"against"`
	Files     []DiffFile `json:"files"`
	Totals    Totals     `json:"totals"`
	Truncated bool       `json:"truncated,omitempty"`
	Next      string     `json:"next,omitempty"`
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)

// DiffSummary says what changed and where, without the patch: per file the
// status, line counts and hunk line ranges (with the declaration git names for
// each), including untracked files. An agent reads the ranges it needs, or
// asks get_pr_context for symbols, risk and tests.
func DiffSummary(ctx context.Context, dir string, in DiffIn) (DiffOut, error) {
	from, label, err := diffBase(ctx, dir, in.Base)
	if err != nil {
		return DiffOut{}, err
	}
	paths, err := cleanPaths(in.Paths)
	if err != nil {
		return DiffOut{}, err
	}
	names, err := nameStatus(ctx, dir, from, paths)
	if err != nil {
		return DiffOut{}, err
	}
	counts, err := numstatPaths(ctx, dir, from, paths)
	if err != nil {
		return DiffOut{}, err
	}
	res := DiffOut{Against: label, Files: []DiffFile{}}
	for _, n := range names {
		c := counts[n.path]
		res.Files = append(res.Files, DiffFile{Path: n.path, Status: n.status, Added: c.added, Removed: c.removed, Binary: c.binary})
	}
	untracked, err := untrackedPaths(ctx, dir, paths)
	if err != nil {
		return DiffOut{}, err
	}
	for _, p := range untracked {
		lines, binary := countLines(dir, p)
		f := DiffFile{Path: p, Status: "?", Added: lines, Binary: binary}
		if lines > 0 {
			f.Hunks = []Hunk{{New: rangeOf(1, lines)}}
		}
		res.Files = append(res.Files, f)
	}
	slices.SortFunc(res.Files, func(a, b DiffFile) int { return strings.Compare(a.Path, b.Path) })
	res.Totals.Files = len(res.Files)
	for _, f := range res.Files {
		res.Totals.Added += f.Added
		res.Totals.Removed += f.Removed
		if f.Status == "?" {
			res.Totals.Untracked++
		}
	}
	if len(res.Files) > MaxFiles {
		res.Files, res.Truncated = res.Files[:MaxFiles], true
	}
	for i := range res.Files {
		f := &res.Files[i]
		if f.Status == "?" || f.Binary {
			continue
		}
		hunks, err := hunksOf(ctx, dir, from, f.Path)
		if err != nil {
			return DiffOut{}, err
		}
		if len(hunks) > MaxHunks {
			f.HunksOmitted, hunks = len(hunks)-MaxHunks, hunks[:MaxHunks]
		}
		f.Hunks = hunks
	}
	if len(res.Files) > 0 {
		res.Next = "Read only the listed ranges. get_pr_context (no arguments) adds changed symbols, risk and covering tests; verify_plan lists what to run."
	}
	return res, nil
}

// diffBase resolves what the working tree is compared with: HEAD, the empty
// tree before the first commit, or the merge base with a named revision.
func diffBase(ctx context.Context, dir, base string) (from, label string, err error) {
	if base == "" {
		if head(ctx, dir) == "" {
			return emptyTree, "the empty tree (no commit yet)", nil
		}
		return "HEAD", "HEAD", nil
	}
	if err := gitx.ValidateRev(base); err != nil {
		return "", "", fmt.Errorf("%w: base: %w", ErrInvalid, err)
	}
	out, err := git(ctx, dir, "merge-base", base, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("%w: no common ancestor of %s and HEAD: %w", ErrInvalid, base, err)
	}
	mb := strings.TrimSpace(string(out))
	return mb, fmt.Sprintf("where HEAD left %s (%s)", base, short(mb)), nil
}

func cleanPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		c, err := CleanPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

type nameEntry struct{ path, status string }

func nameStatus(ctx context.Context, dir, from string, paths []string) ([]nameEntry, error) {
	args := append([]string{"diff", "--name-status", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", from, "--"}, paths...)
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	parts := splitZ(out)
	var res []nameEntry
	for i := 0; i+1 < len(parts); i += 2 {
		s := parts[i]
		switch {
		case strings.HasPrefix(s, "A"):
			s = "A"
		case strings.HasPrefix(s, "D"):
			s = "D"
		default:
			s = "M"
		}
		res = append(res, nameEntry{path: parts[i+1], status: s})
	}
	return res, nil
}

func numstatPaths(ctx context.Context, dir, from string, paths []string) (map[string]lineCount, error) {
	args := append([]string{"diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", from, "--"}, paths...)
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	res := map[string]lineCount{}
	for _, rec := range splitZ(out) {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) == 3 {
			res[f[2]] = lineCount{added: atoi(f[0]), removed: atoi(f[1]), binary: f[0] == "-"}
		}
	}
	return res, nil
}

func untrackedPaths(ctx context.Context, dir string, paths []string) ([]string, error) {
	args := append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, paths...)
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	list := splitZ(out)
	if len(list) > maxUntracked {
		list = list[:maxUntracked]
	}
	return list, nil
}

// hunksOf lists the hunks of one file against from, from a zero-context
// diff so each hunk is exactly the changed lines.
func hunksOf(ctx context.Context, dir, from, path string) ([]Hunk, error) {
	out, err := git(ctx, dir, "diff", "--unified=0", "--no-color", "--no-renames", "--no-ext-diff", "--no-textconv", from, "--", path)
	if err != nil {
		return nil, err
	}
	var hunks []Hunk
	for _, line := range strings.Split(string(out), "\n") {
		m := hunkRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		h := Hunk{New: span(m[3], m[4]), In: safeText(m[5], 80)}
		if m[2] != "0" {
			h.Old = span(m[1], m[2])
		}
		if m[4] == "0" { // a pure deletion: the line after which it was removed
			h.New = "after " + m[3]
		}
		hunks = append(hunks, h)
	}
	return hunks, nil
}

// span turns a hunk's start and optional length into "a-b" (or "a").
func span(start, length string) string {
	s, n := atoi(start), 1
	if length != "" {
		n = atoi(length)
	}
	if n <= 0 {
		return start
	}
	return rangeOf(s, s+n-1)
}

func rangeOf(a, b int) string {
	if a >= b {
		return fmt.Sprint(a)
	}
	return fmt.Sprintf("%d-%d", a, b)
}
