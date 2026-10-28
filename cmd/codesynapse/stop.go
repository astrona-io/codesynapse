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
	Problems []string // syntax or formatting problems of changed Go files
}

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
// commits not yet pushed, fingerprints them and checks the Go ones.
func stopChanges(ctx context.Context, root string) stopChange {
	paths := changedPaths(ctx, root)
	// Three dots: from the merge base, so commits the upstream gained since
	// (a checkout behind after others merged) are not taken for this session's.
	if out, err := gitx.Run(ctx, root, "diff", "--name-only", "--no-renames", "@{upstream}...HEAD"); err == nil {
		paths = append(paths, strings.Fields(string(out))...)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	h := sha256.New()
	var c stopChange
	var goFiles []string
	for _, p := range paths {
		if !stopRelevant(p) {
			continue
		}
		c.Files++
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
