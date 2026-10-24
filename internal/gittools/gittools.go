// Package gittools answers the questions developers put to git as small,
// deterministic JSON instead of raw command output: where the checkout
// stands (Status), what changed and where (DiffSummary), who last changed
// some lines (Blame) and what happened to a path lately (Log). An agent that
// would otherwise run git status, diff, blame and log, and then open files to
// make sense of them, gets the same facts in a few hundred tokens.
//
// Everything runs through gitx.Run (hardened, no shell, no hooks) and is
// read-only. Paths are repository-relative; limits bound every answer. The
// answers hold no file contents and no e-mail addresses. Commit subjects and
// hunk headings pass redact.Summary.
package gittools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Limits on every answer, so a huge checkout cannot produce a huge reply.
const (
	MaxFiles      = 200 // files listed by Status and DiffSummary
	MaxHunks      = 40  // hunks listed per file
	MaxBlameLines = 400 // lines a Blame range may cover
	MaxLogEntries = 50
	defaultLog    = 15
	maxUntracked  = 100
	maxCountBytes = 1 << 20 // untracked files above this are not read to count lines
	subjectLimit  = 120
	nameLimit     = 60
)

// emptyTree is the hash of the empty tree: the base of a repository without
// a commit.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// ErrInvalid reports an argument the tools refuse (a path outside the
// repository, a bad range, an unusable revision).
var ErrInvalid = errors.New("invalid argument")

// CleanPath checks that p is a repository-relative path with no way out of
// the checkout and returns it in slash form.
func CleanPath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) || filepath.IsAbs(p) || strings.HasPrefix(p, "-") || strings.HasPrefix(p, ":") {
		return "", fmt.Errorf("%w: path %q must be a plain repository-relative path", ErrInvalid, p)
	}
	c := filepath.ToSlash(filepath.Clean(p))
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%w: path %q leaves the repository", ErrInvalid, p)
	}
	return c, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitx.Run(ctx, dir, args...)
}

// head returns the commit HEAD points to ("" in a repository without one).
func head(ctx context.Context, dir string) string {
	out, err := git(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// splitZ splits NUL-terminated git output.
func splitZ(b []byte) []string {
	parts := strings.Split(string(bytes.TrimRight(b, "\x00")), "\x00")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s) //nolint:errcheck // "-" for binary files means 0
	return n
}

// countLines counts the lines of an untracked file, or reports it as
// binary or too large to read.
func countLines(dir, rel string) (lines int, binary bool) {
	f := filepath.Join(dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(f)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxCountBytes {
		return 0, err == nil && fi.Mode().IsRegular()
	}
	b, err := os.ReadFile(f) //nolint:gosec // a regular file inside the checkout, size-checked
	if err != nil {
		return 0, false
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return 0, true
	}
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n, false
}

func safeText(s string, n int) string { return redact.Summary(clip(strings.TrimSpace(s), n), n) }
