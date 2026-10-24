package gittools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/gitx"
)

// BlameIn is git_blame's input: lines Start to End (inclusive; End defaults
// to Start) of File as it is in the working tree.
type BlameIn struct {
	File  string `json:"file" jsonschema:"repo-relative path"`
	Start int    `json:"start" jsonschema:"first line"`
	End   int    `json:"end,omitempty" jsonschema:"last line (default: start); at most 400 lines"`
}

// BlameBlock is a run of consecutive lines last changed by one commit.
type BlameBlock struct {
	Lines   string `json:"lines"`
	Commit  string `json:"commit"` // "uncommitted" for lines not committed yet
	Author  string `json:"author,omitempty"`
	Date    string `json:"date,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// BlameOut is git_blame's answer.
type BlameOut struct {
	File   string       `json:"file"`
	Blocks []BlameBlock `json:"blocks"`
	Next   string       `json:"next,omitempty"`
}

// Blame says who last changed some lines: one entry per run of lines from the
// same commit, instead of one line of output per source line.
func Blame(ctx context.Context, dir string, in BlameIn) (BlameOut, error) {
	file, err := CleanPath(in.File)
	if err != nil {
		return BlameOut{}, err
	}
	end := in.End
	if end == 0 {
		end = in.Start
	}
	switch {
	case in.Start < 1 || end < in.Start:
		return BlameOut{}, fmt.Errorf("%w: lines %d-%d", ErrInvalid, in.Start, end)
	case end-in.Start+1 > MaxBlameLines:
		return BlameOut{}, fmt.Errorf("%w: ask for at most %d lines at a time", ErrInvalid, MaxBlameLines)
	}
	out, err := git(ctx, dir, "blame", "--line-porcelain", "-w", "-L", fmt.Sprintf("%d,%d", in.Start, end), "--", file)
	if err != nil {
		return BlameOut{}, err
	}
	res := BlameOut{File: file, Blocks: []BlameBlock{}}
	var cur *BlameBlock
	var curSHA string
	var curLine int
	var first, last int
	flush := func() {
		if cur != nil {
			cur.Lines = rangeOf(first, last)
			res.Blocks = append(res.Blocks, *cur)
		}
		cur = nil
	}
	meta := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "\t") { // the source line: its metadata is complete
			sha := meta["sha"]
			if cur == nil || sha != curSHA {
				flush()
				cur, curSHA, first = &BlameBlock{Commit: short(sha)}, sha, curLine
				if strings.Trim(sha, "0") == "" {
					cur.Commit = "uncommitted"
				} else {
					cur.Author = safeText(meta["author"], nameLimit)
					cur.Subject = safeText(meta["summary"], subjectLimit)
					if ts := atoi(meta["author-time"]); ts > 0 {
						cur.Date = time.Unix(int64(ts), 0).UTC().Format(time.DateOnly)
					}
				}
			}
			last = curLine
			meta = map[string]string{}
			continue
		}
		if f := strings.Fields(line); len(f) >= 3 && len(f[0]) == 40 && isHex(f[0]) { // "<sha> <orig> <final> [<n>]"
			meta["sha"], curLine = f[0], atoi(f[2])
			continue
		}
		if k, v, ok := strings.Cut(line, " "); ok {
			meta[k] = v
		}
	}
	flush()
	if len(res.Blocks) > 0 {
		res.Next = "git_log with the same path lists the surrounding history; get_symbol_context shows churn and callers."
	}
	return res, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// LogIn is git_log's input.
type LogIn struct {
	// Path limits the history to commits touching it (a file or directory).
	Path string `json:"path,omitempty" jsonschema:"only commits touching this file or directory (repo-relative)"`
	// Limit is how many commits to list (default 15, at most 50).
	Limit int `json:"limit,omitempty" jsonschema:"commits to list (default 15, at most 50)"`
	// Ref is where to start (default HEAD).
	Ref string `json:"ref,omitempty" jsonschema:"branch, tag or commit to start from (default HEAD)"`
}

// LogEntry is one commit in compact form.
type LogEntry struct {
	Commit  string   `json:"commit"`
	Date    string   `json:"date"`
	Author  string   `json:"author"`
	Subject string   `json:"subject"`
	Files   int      `json:"files"`
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
	Touched []string `json:"touched,omitempty"` // the files changed most, at most three
}

// LogOut is git_log's answer.
type LogOut struct {
	Path    string     `json:"path,omitempty"`
	Commits []LogEntry `json:"commits"`
	Next    string     `json:"next,omitempty"`
}

const (
	recordSep = "\x1e"
	fieldSep  = "\x1f"
)

// Log lists recent commits, one entry each, with how much they changed and
// where, instead of the full messages and patches.
func Log(ctx context.Context, dir string, in LogIn) (LogOut, error) {
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultLog
	case limit < 0 || limit > MaxLogEntries:
		return LogOut{}, fmt.Errorf("%w: limit is 1 to %d", ErrInvalid, MaxLogEntries)
	}
	ref := in.Ref
	if ref == "" {
		ref = "HEAD"
	} else if err := gitx.ValidateRev(ref); err != nil {
		return LogOut{}, fmt.Errorf("%w: ref: %w", ErrInvalid, err)
	}
	res := LogOut{Commits: []LogEntry{}}
	args := []string{"log", "--no-color", "--no-renames", "--no-merges", "--numstat", "-z",
		"--format=" + recordSep + "%h" + fieldSep + "%as" + fieldSep + "%an" + fieldSep + "%s" + fieldSep, "-n", fmt.Sprint(limit), ref, "--"}
	if in.Path != "" {
		p, err := CleanPath(in.Path)
		if err != nil {
			return LogOut{}, err
		}
		res.Path, args = p, append(args, p)
	}
	if head(ctx, dir) == "" {
		return res, nil // no commit yet
	}
	out, err := git(ctx, dir, args...)
	if err != nil {
		return LogOut{}, err
	}
	for _, rec := range strings.Split(string(out), recordSep) {
		if strings.TrimSpace(rec) == "" {
			continue
		}
		head, stats, _ := strings.Cut(rec, fieldSep+"\x00")
		if stats == "" { // -z puts the numstat after the format's terminating NUL
			head, stats, _ = strings.Cut(rec, fieldSep)
		}
		f := strings.Split(head, fieldSep)
		if len(f) < 4 {
			continue
		}
		e := LogEntry{Commit: f[0], Date: f[1], Author: safeText(f[2], nameLimit), Subject: safeText(f[3], subjectLimit)}
		type changed struct {
			path  string
			lines int
		}
		var top []changed
		for _, line := range strings.Split(strings.Trim(stats, "\x00\n"), "\x00") {
			c := strings.SplitN(strings.TrimLeft(line, "\n"), "\t", 3)
			if len(c) != 3 {
				continue
			}
			a, r := atoi(c[0]), atoi(c[1])
			e.Files++
			e.Added += a
			e.Removed += r
			top = append(top, changed{c[2], a + r})
		}
		for range min(3, len(top)) {
			best := 0
			for i, t := range top {
				if t.lines > top[best].lines || t.lines == top[best].lines && t.path < top[best].path {
					best = i
				}
			}
			e.Touched = append(e.Touched, top[best].path)
			top = append(top[:best], top[best+1:]...)
		}
		res.Commits = append(res.Commits, e)
	}
	if len(res.Commits) > 0 {
		res.Next = "git_diff_summary shows what is uncommitted now; file_history and get_symbol_context (churn) cover indexed history."
	}
	return res, nil
}
