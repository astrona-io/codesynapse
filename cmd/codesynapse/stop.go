package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/gitx"
)

// The Stop hook asks for verify_plan and get_pr_context only when the session
// left something they can say anything about: changes to files CodeSynapse
// indexes, other than prose. Local settings (.claude/, .mcp.json), lock
// files and Markdown never trigger it. Go files are also checked here, with
// the standard library (parse and gofmt, no command is run), so a syntax
// error or an unformatted file is reported with its location instead of
// costing the model two tool calls first.

const (
	// maxCheckedGo bounds how many Go files the hook parses.
	maxCheckedGo = 50
	// maxCheckedBytes skips files too large for a hook that must be quick.
	maxCheckedBytes = 1 << 20
	maxProblems     = 10
)

// stopChange is what the Stop hook found to ask about.
type stopChange struct {
	Key      string   // fingerprint of the changed files, "" when there is nothing
	Files    int      // indexed files changed (prose and local settings left out)
	Paths    []string // the first maxNamedFiles of them, sorted
	Problems []string // syntax or formatting problems of changed Go files
}

// maxNamedFiles bounds the files a Stop request names for verify_plan.
const maxNamedFiles = 20

// stopRelevant reports a changed path worth verifying.
func stopRelevant(p string) bool {
	for _, dir := range []string{".claude/", ".git/", ".codex/", ".cursor/"} {
		if strings.HasPrefix(p, dir) {
			return false
		}
	}
	return indexable(p) && path.Ext(p) != ".md"
}

// stopChanges lists the relevant files changed in the working tree or in
// commits not yet pushed, fingerprints them and checks the Go ones. With a
// base (the checkout as the session began) only files the session changed
// since count: what was already dirty, or committed but not pushed, at the
// start is someone else's work or an earlier session's.
func stopChanges(ctx context.Context, root string, base *treeBase) stopChange {
	paths := changedPaths(ctx, root)
	// Three dots: from the merge base, so commits the upstream gained since
	// (a checkout behind after others merged) are not taken for this session's.
	if out, err := gitx.Run(ctx, root, "diff", "--name-only", "--no-renames", "@{upstream}...HEAD"); err == nil {
		paths = append(paths, strings.Fields(string(out))...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	var own map[string]bool
	if base != nil {
		own = map[string]bool{}
		for _, p := range sessionEdits(ctx, root, base) {
			own[p] = true
		}
	}
	h := sha256.New()
	var c stopChange
	var goFiles []string
	for _, p := range paths {
		if !stopRelevant(p) || own != nil && !own[p] {
			continue
		}
		c.Files++
		if len(c.Paths) < maxNamedFiles {
			c.Paths = append(c.Paths, p)
		}
		fmt.Fprintf(h, "%s\x00%s\x00", p, fileSig(root, p)) //nolint:errcheck // hashes never fail
		if path.Ext(p) == ".go" && len(goFiles) < maxCheckedGo {
			goFiles = append(goFiles, p)
		}
	}
	if c.Files == 0 {
		return stopChange{}
	}
	c.Key = hex.EncodeToString(h.Sum(nil))
	for _, p := range goFiles {
		if len(c.Problems) == maxProblems {
			break
		}
		if problem := goProblem(root, p); problem != "" {
			c.Problems = append(c.Problems, problem)
		}
	}
	return c
}

// goProblem parses and formats one Go file the way gofmt does; "" means fine
// (or not checkable: missing, too large, unreadable).
func goProblem(root, p string) string {
	file := filepath.Join(root, filepath.FromSlash(p))
	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxCheckedBytes {
		return ""
	}
	src, err := os.ReadFile(file) //nolint:gosec // a changed file of the checkout
	if err != nil {
		return ""
	}
	if _, err := parser.ParseFile(token.NewFileSet(), p, src, parser.AllErrors|parser.SkipObjectResolution); err != nil {
		msg := strings.SplitN(err.Error(), "\n", 2)[0]
		return msg
	}
	if out, err := format.Source(src); err == nil && !bytes.Equal(out, src) {
		return p + ": not gofmt-formatted"
	}
	return ""
}

// maxEditDirs bounds the directories a session's edits are remembered in.
const maxEditDirs = 32

// noteEditDir remembers the directory of a file the session's own agent
// edited, so the Stop hook knows which checkouts the session worked in. Edits
// of a subagent (agent_id set) are its own: the parent did not make them.
func noteEditDir(s *hookState, in toolHookInput) {
	file := in.ToolInput.FilePath
	if in.AgentID != "" || file == "" {
		return
	}
	if !filepath.IsAbs(file) {
		if in.Cwd == "" {
			return
		}
		file = filepath.Join(in.Cwd, file)
	}
	dir := filepath.Dir(filepath.Clean(file))
	if slices.Contains(s.EditDirs, dir) {
		return
	}
	s.EditDirs = append(s.EditDirs, dir)
	if len(s.EditDirs) > maxEditDirs {
		s.EditDirs = s.EditDirs[len(s.EditDirs)-maxEditDirs:]
	}
}

// editedTrees sorts the checkouts the session's agent edited files in, as
// seen from start, the checkout the session began in (the one the bridge
// answers for): whether it edited start itself, whether it edited any other
// checkout, and which of those are worktrees of the same repository (the
// tools can answer for those when the files are named). Subagent worktrees
// (.claude/worktrees/agent-*) belong to the subagent and are left out.
func editedTrees(ctx context.Context, start string, dirs []string) (inStart, elsewhere bool, siblings []string) {
	common := gitCommonDir(ctx, start)
	seen := map[string]bool{}
	for _, dir := range dirs {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		out, err := gitx.Run(ctx, dir, "rev-parse", "--show-toplevel")
		if err != nil {
			continue // not in a checkout: a scratch file
		}
		root := strings.TrimSpace(string(out))
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		switch {
		case samePath(root, start):
			inStart = true
		case subagentTree(root):
		default:
			elsewhere = true
			if common != "" && gitCommonDir(ctx, root) == common {
				siblings = append(siblings, root)
			}
		}
	}
	slices.Sort(siblings)
	return inStart, elsewhere, siblings
}

// gitCommonDir is the repository a checkout belongs to, shared by all its
// worktrees; "" when git cannot say.
func gitCommonDir(ctx context.Context, root string) string {
	out, err := gitx.Run(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func samePath(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}

// subagentTree reports a worktree Claude Code made for a subagent.
func subagentTree(root string) bool {
	parent := filepath.Dir(root)
	return strings.HasPrefix(filepath.Base(root), "agent-") &&
		filepath.Base(parent) == "worktrees" && filepath.Base(filepath.Dir(parent)) == ".claude"
}
