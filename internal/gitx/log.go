package gitx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Commit is one entry from git log with the files it touched.
type Commit struct {
	Hash        string
	AuthorName  string
	AuthorEmail string
	Time        string // RFC 3339
	Subject     string
	Files       []FileStat
}

// FileStat is the numstat line for one file in a commit.
type FileStat struct {
	Path    string
	Added   int
	Removed int
}

// Record and field separators that cannot appear in git's formatted output.
const (
	recSep   = "\x1e"
	fieldSep = "\x1f"
)

// Log returns up to limit commits reachable from HEAD, newest first.
func Log(ctx context.Context, dir string, limit int) ([]Commit, error) {
	if limit <= 0 {
		return nil, nil
	}
	format := recSep + strings.Join([]string{"%H", "%an", "%ae", "%aI", "%s"}, fieldSep)
	out, err := Run(ctx, dir, "log", "--no-color", "--no-merges", "--no-renames",
		"--numstat", "--format="+format, "-n", strconv.Itoa(limit), "HEAD", "--")
	if err != nil {
		return nil, err
	}
	return ParseLog(out)
}

// ParseLog parses output produced by Log's format.
func ParseLog(out []byte) ([]Commit, error) {
	var commits []Commit
	for _, rec := range bytes.Split(out, []byte(recSep)) {
		if len(bytes.TrimSpace(rec)) == 0 {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(rec))
		sc.Buffer(make([]byte, 0, 64*1024), maxDiffLine)
		if !sc.Scan() {
			continue
		}
		fields := strings.Split(sc.Text(), fieldSep)
		if len(fields) != 5 {
			return nil, fmt.Errorf("malformed log header: %q", sc.Text())
		}
		c := Commit{Hash: fields[0], AuthorName: fields[1], AuthorEmail: fields[2], Time: fields[3], Subject: fields[4]}
		for sc.Scan() {
			parts := strings.SplitN(sc.Text(), "\t", 3)
			if len(parts) != 3 {
				continue
			}
			// Binary files report "-" for both counts.
			added, _ := strconv.Atoi(parts[0])   //nolint:errcheck // "-" for binary files means 0
			removed, _ := strconv.Atoi(parts[1]) //nolint:errcheck // same as above
			c.Files = append(c.Files, FileStat{Path: unquotePath(parts[2]), Added: added, Removed: removed})
		}
		commits = append(commits, c)
	}
	return commits, nil
}

// unquotePath decodes the C-style quoting git applies to a path in numstat
// output when it holds a control character, a double quote, a backslash or
// (with the default core.quotePath) a non-ASCII byte. The escapes git writes
// (\t, \n, \", \\ and three-digit octal bytes) are also Go string escapes,
// so strconv.Unquote decodes them, as diff.go does for diff headers. An
// unquoted path is returned as is, and so is a malformed quoted one, which
// cannot match a file anyway.
func unquotePath(s string) string {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}
