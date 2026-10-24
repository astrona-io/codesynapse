package gittools

import (
	"context"
	"slices"
	"strings"
)

// StatusFile is one changed path.
type StatusFile struct {
	Path string `json:"path"`
	// Status is M modified, A added, D deleted, R renamed, U conflicted,
	// ? untracked.
	Status   string `json:"status"`
	Staged   bool   `json:"staged,omitempty"`
	Unstaged bool   `json:"unstaged,omitempty"`
	Added    int    `json:"added,omitempty"`
	Removed  int    `json:"removed,omitempty"`
	Binary   bool   `json:"binary,omitempty"`
	From     string `json:"from,omitempty"` // the old path of a rename
}

// StatusOut is git_status's answer.
type StatusOut struct {
	Branch    string       `json:"branch,omitempty"` // empty when detached
	Head      string       `json:"head,omitempty"`
	Upstream  string       `json:"upstream,omitempty"`
	Ahead     int          `json:"ahead,omitempty"`
	Behind    int          `json:"behind,omitempty"`
	Clean     bool         `json:"clean"`
	Files     []StatusFile `json:"files"`
	Totals    Totals       `json:"totals"`
	Truncated bool         `json:"truncated,omitempty"`
	Next      string       `json:"next,omitempty"`
}

// Totals sum a change.
type Totals struct {
	Files     int `json:"files"`
	Added     int `json:"added"`
	Removed   int `json:"removed"`
	Untracked int `json:"untracked,omitempty"`
}

// Status is git status and git diff --stat in one answer: the branch and how
// far it is from its upstream, and every changed path with its line counts.
func Status(ctx context.Context, dir string) (StatusOut, error) {
	out, err := git(ctx, dir, "status", "--porcelain=v2", "--branch", "-z", "--untracked-files=all")
	if err != nil {
		return StatusOut{}, err
	}
	res := StatusOut{Files: []StatusFile{}, Head: short(head(ctx, dir))}
	parts := splitZ(out)
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		switch {
		case strings.HasPrefix(p, "# branch.head "):
			if b := strings.TrimPrefix(p, "# branch.head "); b != "(detached)" {
				res.Branch = b
			}
		case strings.HasPrefix(p, "# branch.upstream "):
			res.Upstream = strings.TrimPrefix(p, "# branch.upstream ")
		case strings.HasPrefix(p, "# branch.ab "):
			f := strings.Fields(strings.TrimPrefix(p, "# branch.ab "))
			if len(f) == 2 {
				res.Ahead, res.Behind = atoi(strings.TrimPrefix(f[0], "+")), atoi(strings.TrimPrefix(f[1], "-"))
			}
		case strings.HasPrefix(p, "1 "): // ordinary change: 1 XY sub mH mI mW hH hI path
			f := strings.SplitN(p, " ", 9)
			if len(f) == 9 {
				res.Files = append(res.Files, entry(f[1], f[8], ""))
			}
		case strings.HasPrefix(p, "2 "): // rename or copy: ... Rscore path, then the old path
			f := strings.SplitN(p, " ", 10)
			if len(f) == 10 && i+1 < len(parts) {
				i++
				res.Files = append(res.Files, entry(f[1], f[9], parts[i]))
			}
		case strings.HasPrefix(p, "u "): // conflict
			f := strings.SplitN(p, " ", 11)
			if len(f) == 11 {
				res.Files = append(res.Files, StatusFile{Path: f[10], Status: "U", Unstaged: true})
			}
		case strings.HasPrefix(p, "? "):
			res.Files = append(res.Files, StatusFile{Path: strings.TrimPrefix(p, "? "), Status: "?"})
		}
	}
	res.Clean = len(res.Files) == 0
	slices.SortFunc(res.Files, func(a, b StatusFile) int { return strings.Compare(a.Path, b.Path) })
	counts, err := numstat(ctx, dir, headOrEmpty(ctx, dir))
	if err != nil {
		return StatusOut{}, err
	}
	untracked := 0
	for i := range res.Files {
		f := &res.Files[i]
		if f.Status == "?" {
			untracked++
			if len(res.Files) <= maxUntracked+MaxFiles {
				f.Added, f.Binary = countLines(dir, f.Path)
			}
		} else if c, ok := counts[f.Path]; ok {
			f.Added, f.Removed, f.Binary = c.added, c.removed, c.binary
		}
		res.Totals.Added += f.Added
		res.Totals.Removed += f.Removed
	}
	res.Totals.Files, res.Totals.Untracked = len(res.Files), untracked
	if len(res.Files) > MaxFiles {
		res.Files, res.Truncated = res.Files[:MaxFiles], true
	}
	switch {
	case res.Clean && res.Behind > 0:
		res.Next = "The checkout is clean but behind its upstream; fetch and pull before editing."
	case !res.Clean:
		res.Next = "git_diff_summary lists the changed line ranges; get_pr_context analyses the change (symbols, risk, tests); verify_plan lists what to run."
	}
	return res, nil
}

// entry builds a StatusFile from a porcelain v2 XY code (index, worktree).
func entry(xy, path, from string) StatusFile {
	f := StatusFile{Path: path, From: from, Staged: xy[0] != '.', Unstaged: len(xy) > 1 && xy[1] != '.'}
	code := xy[0]
	if code == '.' && len(xy) > 1 {
		code = xy[1]
	}
	switch code {
	case 'A', 'C':
		f.Status = "A"
	case 'D':
		f.Status = "D"
	case 'R':
		f.Status = "R"
	default:
		f.Status = "M"
	}
	return f
}

type lineCount struct {
	added, removed int
	binary         bool
}

// headOrEmpty is HEAD, or the empty tree in a repository without a commit.
func headOrEmpty(ctx context.Context, dir string) string {
	if head(ctx, dir) == "" {
		return emptyTree
	}
	return "HEAD"
}

// numstat counts the added and removed lines per path between base and the
// working tree (index and unstaged changes together).
func numstat(ctx context.Context, dir, base string) (map[string]lineCount, error) {
	out, err := git(ctx, dir, "diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", base, "--")
	if err != nil {
		return nil, err
	}
	res := map[string]lineCount{}
	for _, rec := range splitZ(out) {
		f := strings.SplitN(rec, "\t", 3)
		if len(f) != 3 {
			continue
		}
		res[f[2]] = lineCount{added: atoi(f[0]), removed: atoi(f[1]), binary: f[0] == "-"}
	}
	return res, nil
}
