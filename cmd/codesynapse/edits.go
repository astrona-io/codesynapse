package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

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

// treeBase is the checkout as a session began: HEAD and the files already
// changed, with their size and modification time, so the session's own
// edits can be told apart from what was there.
type treeBase struct {
	Head  string            `json:"head,omitempty"`
	Dirty map[string]string `json:"dirty,omitempty"`
}

func baseOf(ctx context.Context, root string) *treeBase {
	b := &treeBase{Head: gitx.HeadCommit(ctx, root), Dirty: map[string]string{}}
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
// changed now unless exactly as they were at the start, and those of
// commits made since.
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
	for _, p := range changedPaths(ctx, root) {
		if sig, was := b.Dirty[p]; !was || sig != fileSig(root, p) {
			add(p)
		}
	}
	if head := gitx.HeadCommit(ctx, root); b.Head != "" && head != "" && head != b.Head && gitx.ValidateRev(b.Head) == nil {
		if names, err := gitx.Run(ctx, root, "diff", "--name-only", "--no-renames", b.Head, "HEAD"); err == nil {
			for _, p := range strings.Split(strings.TrimSpace(string(names)), "\n") {
				if p != "" {
					add(p)
				}
			}
		}
	}
	return out
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
