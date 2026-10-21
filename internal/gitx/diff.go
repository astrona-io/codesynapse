package gitx

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// LineRange is an inclusive range of line numbers on the new side of a diff.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// FileChange describes the changes to one file in a diff.
type FileChange struct {
	// Path is the repository-relative path on the new side (old side for deletions).
	Path    string      `json:"path"`
	OldPath string      `json:"old_path,omitempty"`
	Added   bool        `json:"added,omitempty"`
	Deleted bool        `json:"deleted,omitempty"`
	Ranges  []LineRange `json:"ranges,omitempty"`
}

// maxDiffLine bounds a single diff line; longer lines (minified assets) are
// skipped by the scanner rather than exhausting memory.
const maxDiffLine = 1 << 20

// ParseDiff parses unified diff output into per-file changes. Pure deletions
// within a file are recorded as a one-line range at the deletion point so the
// surrounding symbol is still considered changed.
func ParseDiff(diff []byte) ([]FileChange, error) {
	var (
		out []FileChange
		cur *FileChange
	)
	flush := func() {
		if cur != nil && cur.Path != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), maxDiffLine)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &FileChange{}
		case strings.HasPrefix(line, "--- "):
			if cur == nil {
				cur = &FileChange{}
			}
			p := stripPrefix(strings.TrimPrefix(line, "--- "))
			if p == "" {
				cur.Added = true
			} else {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if cur == nil {
				cur = &FileChange{}
			}
			p := stripPrefix(strings.TrimPrefix(line, "+++ "))
			if p == "" {
				cur.Deleted = true
				cur.Path = cur.OldPath
			} else {
				cur.Path = p
			}
			if cur.OldPath == cur.Path {
				cur.OldPath = ""
			}
		case strings.HasPrefix(line, "@@ "):
			if cur == nil {
				return nil, fmt.Errorf("hunk header before file header: %q", line)
			}
			r, err := parseHunk(line)
			if err != nil {
				return nil, err
			}
			if !cur.Deleted {
				cur.Ranges = append(cur.Ranges, r)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scanning diff: %w", err)
	}
	flush()
	return out, nil
}

// stripPrefix turns "a/path" or "b/path" into "path" and "/dev/null" into "".
func stripPrefix(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if p == "/dev/null" {
		return ""
	}
	p = strings.Trim(p, `"`)
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

// parseHunk parses "@@ -a,b +c,d @@ ..." and returns the new-side range.
func parseHunk(line string) (LineRange, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
		return LineRange{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	spec := strings.TrimPrefix(fields[2], "+")
	startStr, countStr, hasCount := strings.Cut(spec, ",")
	start, err := strconv.Atoi(startStr)
	if err != nil || start < 0 {
		return LineRange{}, fmt.Errorf("malformed hunk start in %q", line)
	}
	count := 1
	if hasCount {
		count, err = strconv.Atoi(countStr)
		if err != nil || count < 0 {
			return LineRange{}, fmt.Errorf("malformed hunk count in %q", line)
		}
	}
	if count == 0 {
		// Pure deletion: git reports the line before the removed block.
		if start == 0 {
			start = 1
		}
		return LineRange{Start: start, End: start}, nil
	}
	return LineRange{Start: start, End: start + count - 1}, nil
}
