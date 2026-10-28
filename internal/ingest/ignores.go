package ingest

import (
	"bufio"
	"context"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

const (
	ignoreMarker = "codesynapse:ignore"
	// maxIgnoreFile bounds what is read per file; findings further down a
	// larger file keep their status.
	maxIgnoreFile = 1 << 20
	maxIgnoreLine = 64 << 10
	maxIgnoreText = 200
)

// ignoreAliases pairs earlier rule IDs a comment may still name with the
// current ones. Pairs rather than a map: the secret detector reads a map entry
// keyed by a secret-looking name as an assignment.
var ignoreAliases = [][2]string{{"sec-yaml-plain-secret", "yaml-hardcoded-secret"}}

// commentStarts are the comment openers a marker must follow, so a string
// literal that happens to contain the marker does not count.
var commentStarts = []string{"//", "#", "/*", "--", "<!--", ";", "*", "{{/*", "{{-/*"}

// markInlineIgnores records the reason of a codesynapse:ignore comment on a
// finding's line or the line above it. The finding stays in the graph either
// way; the reason only suppresses it under an approved inline_ignores policy.
func markInlineIgnores(ctx context.Context, root string, g *model.Graph, policy *repoconfig.Policy) {
	byFile := map[string][]*model.Node{}
	for _, n := range g.Nodes() {
		if n.Kind != model.KindFinding || n.File == "" || n.StartLine < 1 || !policy.PathAllowed(n.File) {
			continue
		}
		if stored, ok := g.Node(n.ID); ok {
			byFile[n.File] = append(byFile[n.File], stored)
		}
	}
	if len(byFile) == 0 {
		return
	}
	dir, err := os.OpenRoot(root)
	if err != nil {
		return
	}
	defer func() { _ = dir.Close() }()
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		if ctx.Err() != nil {
			return
		}
		findings := byFile[file]
		last := 0
		for _, n := range findings {
			last = max(last, n.StartLine)
		}
		lines := readLines(dir, file, last)
		for _, n := range findings {
			for _, at := range []int{n.StartLine, n.StartLine - 1} {
				if at < 1 || at > len(lines) {
					continue
				}
				if reason, ok := ignoreComment(lines[at-1], n.Rule); ok {
					n.Ignore = reason
					break
				}
			}
		}
	}
}

// readLines returns at most the first upto lines of a regular file inside
// dir; a symlink, special file or unreadable file gives none.
func readLines(dir *os.Root, file string, upto int) []string {
	name := path.Clean(file)
	if strings.HasPrefix(name, "../") || name == ".." || path.IsAbs(name) {
		return nil
	}
	info, err := dir.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	f, err := dir.Open(name)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	sc := bufio.NewScanner(io.LimitReader(f, maxIgnoreFile))
	sc.Buffer(make([]byte, 0, 4096), maxIgnoreLine)
	var lines []string
	for len(lines) < upto && sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines
}

// ignoreComment parses "codesynapse:ignore <rule> <reason>" in a comment.
// The rule must name the finding's rule and the reason must not be empty.
func ignoreComment(line, rule string) (string, bool) {
	i := strings.Index(line, ignoreMarker)
	if i < 0 || !afterComment(line[:i]) {
		return "", false
	}
	fields := strings.Fields(line[i+len(ignoreMarker):])
	if len(fields) < 2 {
		return "", false
	}
	named := fields[0]
	for _, alias := range ignoreAliases {
		if named == alias[0] {
			named = alias[1]
		}
	}
	if named != rule {
		return "", false
	}
	reason := strings.Join(fields[1:], " ")
	for _, end := range []string{"*/}}", "-*/}}", "*/", "-->"} {
		reason = strings.TrimSpace(strings.TrimSuffix(reason, end))
	}
	reason = redact.Summary(redact.Free(reason), maxIgnoreText)
	return reason, reason != ""
}

func afterComment(before string) bool {
	before = strings.TrimSpace(before)
	for _, start := range commentStarts {
		if j := strings.LastIndex(before, start); j >= 0 && strings.TrimSpace(before[j+len(start):]) == "" {
			return true
		}
	}
	return false
}
