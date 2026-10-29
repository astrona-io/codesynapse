package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/ingest/lang"
)

// Context vs. edits (docs/PLAN.md, "Context vs. edit comparison"): of the
// files a session changed, how many had a CodeSynapse answer of the same
// session pointed to? The task eval measures that on old commits; this
// measures it on the work agents do. The paths stay in the session's state
// file on this machine; only the two counts are reported.

// maxBaseFiles bounds the changed files remembered at session start.
const maxBaseFiles = 2000

// maxOwnCommits bounds the HEAD reflog entries read for the session's commits.
const maxOwnCommits = 500

// treeBase is the checkout as a session began: HEAD, when (Unix seconds) and
// the files already changed, with their size and modification time, so the
// session's own edits can be told apart from what was there.
type treeBase struct {
	Head  string            `json:"head,omitempty"`
	At    int64             `json:"at,omitempty"`
	Dirty map[string]string `json:"dirty,omitempty"`
}

func baseOf(ctx context.Context, root string) *treeBase {
	b := &treeBase{Head: gitx.HeadCommit(ctx, root), At: time.Now().Unix(), Dirty: map[string]string{}}
	for _, p := range changedPaths(ctx, root) {
		if len(b.Dirty) == maxBaseFiles {
			break
		}
		b.Dirty[p] = fileSig(root, p)
	}
	return b
}

// changedPaths lists the changed and untracked files of the working tree.
func changedPaths(ctx context.Context, root string) []string {
	status, err := gitx.Run(ctx, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	var out []string
	entries := strings.Split(string(status), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		out = append(out, e[3:])
		if e[0] == 'R' || e[0] == 'C' {
			i++ // a rename or copy is followed by its source path
		}
	}
	return out
}

func fileSig(root, p string) string {
	st, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
	if err != nil {
		return "gone"
	}
	return fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
}

// indexable reports a file CodeSynapse could have pointed to: source in a
// language it indexes, Markdown or YAML.
func indexable(p string) bool {
	switch path.Ext(p) {
	case ".go", ".md", ".yaml", ".yml":
		return true
	}
	return lang.Language(p) != ""
}

// sessionEdits lists the indexable files the session changed since b: those
// changed now unless exactly as they were at the start or as a merge in
// progress brings them, and those of commits the session made. A merge or
// pull moves HEAD over commits made elsewhere; diffing the start against HEAD
// would take every file they touch for the session's own.
func sessionEdits(ctx context.Context, root string, b *treeBase) []string {
	if b == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] && indexable(p) {
			seen[p] = true
			out = append(out, p)
		}
	}
	incoming := mergeIncoming(ctx, root)
	for _, p := range changedPaths(ctx, root) {
		if incoming[p] {
			continue
		}
		if sig, was := b.Dirty[p]; !was || sig != fileSig(root, p) {
			add(p)
		}
	}
	if head := gitx.HeadCommit(ctx, root); b.Head != "" && head != "" && head != b.Head && gitx.ValidateRev(b.Head) == nil {
		for _, c := range ownCommits(ctx, root, b) {
			// --cc: of a merge commit, only the paths it resolved by hand.
			names, err := gitx.Run(ctx, root, "diff-tree", "-r", "--cc", "--root", "--name-only", "--no-commit-id", "--no-renames", c)
			if err != nil {
				continue
			}
			for _, p := range strings.Split(strings.TrimSpace(string(names)), "\n") {
				if p != "" {
					add(p)
				}
			}
		}
	}
	return out
}

// ownCommits lists the commits the session made, newest first: the HEAD
// reflog entries since b that git wrote for a commit ("commit", "commit
// (amend)", "commit (merge)"). Merges, pulls, rebases, resets and checkouts
// log other actions, so the commits they bring in are not listed. Reading
// stops at the entry that left HEAD at the start commit before the session
// began. Without a reflog nothing is listed: committed edits then go
// uncounted rather than miscounted.
func ownCommits(ctx context.Context, root string, b *treeBase) []string {
	log, err := gitx.Run(ctx, root, "log", "-g", "--date=unix", "--format=%H%x00%gd%x00%gs", "-n", strconv.Itoa(maxOwnCommits), "HEAD", "--")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		f := strings.SplitN(line, "\x00", 3)
		if len(f) != 3 || gitx.ValidateRev(f[0]) != nil {
			continue
		}
		at, ok := reflogTime(f[1])
		if b.At > 0 && ok && at < b.At {
			break // written before the session began
		}
		if f[0] == b.Head && (b.At == 0 || ok && at <= b.At) {
			break // HEAD as the session found it
		}
		if strings.HasPrefix(f[2], "commit:") || strings.HasPrefix(f[2], "commit (") {
			out = append(out, f[0])
		}
	}
	return out
}

// reflogTime reads the Unix time of a "HEAD@{1700000000}" selector.
func reflogTime(sel string) (int64, bool) {
	_, rest, ok := strings.Cut(sel, "@{")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSuffix(rest, "}"), 10, 64)
	return n, err == nil
}

// mergeIncoming lists, while a merge is in progress, the changed paths that
// are exactly as the merged commit has them: what the merge brings in, not
// what the session wrote. Conflicted and untracked files are not listed.
func mergeIncoming(ctx context.Context, root string) map[string]bool {
	if _, err := gitx.Run(ctx, root, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err != nil {
		return nil
	}
	differs := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "--no-renames", "-z", "MERGE_HEAD", "--"},
		{"ls-files", "-z", "--others", "--exclude-standard"},
	} {
		names, err := gitx.Run(ctx, root, args...)
		if err != nil {
			return nil
		}
		for _, p := range strings.Split(string(names), "\x00") {
			differs[p] = true
		}
	}
	incoming := map[string]bool{}
	for _, p := range changedPaths(ctx, root) {
		if !differs[p] {
			incoming[p] = true
		}
	}
	return incoming
}

// found counts the edited files an answer of this repository had named.
func found(edited []string, ranges []answerRange, repo string) int {
	n := 0
	for _, p := range edited {
		for _, r := range ranges {
			if (r.Repo == "" || r.Repo == repo) && (p == r.File || strings.HasSuffix(p, "/"+r.File) || strings.HasSuffix(r.File, "/"+p)) {
				n++
				break
			}
		}
	}
	return n
}
