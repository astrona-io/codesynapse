// Package codeowners reads a repository's CODEOWNERS file and answers who
// owns a path (security roadmap item 10): findings and changes then name
// the team that can act on them.
//
// The syntax is GitHub's (GitLab's is a superset): one pattern per line in
// gitignore style, followed by owners; the last matching line wins, and a
// line without owners leaves its paths unowned. GitLab sections ([Name])
// are read as plain lines, without their default owners. Owners are kept
// only as @user or @org/team handles: an e-mail address is personal data
// and is dropped. The file is untrusted, so it is bounded and every
// pattern is compiled to a regular expression that cannot backtrack.
package codeowners

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

const (
	// maxFileBytes is GitHub's own limit for a CODEOWNERS file.
	maxFileBytes   = 3 << 20
	maxRules       = 1000
	maxOwners      = 20
	maxPatternLen  = 512
	maxLineLen     = 4096
	maxOwnerLength = 100
)

// Locations are where GitHub and GitLab look, in their order.
var Locations = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS", ".gitlab/CODEOWNERS"}

var handleRe = regexp.MustCompile(`^@[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?(?:/[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

// Read parses the first CODEOWNERS file under root, returning its rules and
// its repository-relative path, or nothing when there is none. A symlink or
// an oversized file is not read.
func Read(root string) ([]model.OwnerRule, string) {
	for _, loc := range Locations {
		p := filepath.Join(root, filepath.FromSlash(loc))
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() > maxFileBytes {
			continue
		}
		data, err := os.ReadFile(p) //nolint:gosec // a fixed name under the repository root, checked above
		if err != nil {
			continue
		}
		return Parse(data), loc
	}
	return nil, ""
}

// Parse reads CODEOWNERS text.
func Parse(data []byte) []model.OwnerRule {
	var out []model.OwnerRule
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), maxLineLen)
	for sc.Scan() && len(out) < maxRules {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "^[") {
			continue
		}
		fields := strings.Fields(line)
		pattern := strings.ReplaceAll(fields[0], `\ `, " ")
		if len(pattern) > maxPatternLen || strings.HasPrefix(pattern, "!") {
			continue // negation is not supported by GitHub either
		}
		r := model.OwnerRule{Pattern: pattern, Owners: []string{}}
		for _, o := range fields[1:] {
			if len(r.Owners) < maxOwners && len(o) <= maxOwnerLength && handleRe.MatchString(o) {
				r.Owners = append(r.Owners, o)
			}
		}
		if compile(pattern) != nil {
			out = append(out, r)
		}
	}
	return out
}

// Set answers who owns a path.
type Set struct {
	rules []model.OwnerRule
	res   []*regexp.Regexp
}

// Compile prepares rules for lookups; nil rules give a Set that owns nothing.
func Compile(rules []model.OwnerRule) *Set {
	s := &Set{}
	for _, r := range rules {
		if re := compile(r.Pattern); re != nil {
			s.rules = append(s.rules, r)
			s.res = append(s.res, re)
		}
	}
	return s
}

// Empty reports a Set without rules: the repository has no CODEOWNERS.
func (s *Set) Empty() bool { return s == nil || len(s.rules) == 0 }

// Owners are the owners of a repository-relative path: those of the last
// rule that matches it, or none.
func (s *Set) Owners(path string) []string {
	if s == nil {
		return nil
	}
	path = strings.TrimPrefix(filepath.ToSlash(path), "/")
	for i := len(s.res) - 1; i >= 0; i-- {
		if s.res[i].MatchString(path) {
			return s.rules[i].Owners
		}
	}
	return nil
}

// compile turns a gitignore-style pattern into an anchored regular
// expression over slash paths, or nil when it cannot be one.
func compile(pattern string) *regexp.Regexp {
	p := pattern
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	// A slash at the start or in the middle anchors to the root; otherwise
	// the pattern matches at any depth.
	anchored := strings.HasPrefix(p, "/") || strings.Contains(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	if !anchored {
		b.WriteString("(?:.*/)?")
	}
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "/**") && i+3 == len(p):
			b.WriteString("/.*")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	last := p[strings.LastIndex(p, "/")+1:]
	switch {
	case dirOnly:
		b.WriteString("/.*$")
	case strings.Contains(last, "*") && last != "**":
		// docs/* owns the files directly in docs/, not nested ones.
		b.WriteString("$")
	default:
		// A name matches the file itself or everything under a directory of
		// that name.
		b.WriteString("(?:/.*)?$")
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
