package runtimefacts

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// Limits on what one report may hold. Reports come from CI artifacts and
// local files, so they are bounded like any other untrusted input.
const (
	MaxReportBytes = 64 << 20
	MaxBlocks      = 1_000_000
	MaxTestCases   = 200_000
	maxPathLen     = 4096
	maxNameLen     = 1024
)

// ErrTooLarge reports a report over the limits above.
var ErrTooLarge = errors.New("report exceeds the size limits")

// Block is a measured range of one file: a Go coverage block (statements)
// or one LCOV line.
type Block struct {
	File      string
	StartLine int
	EndLine   int
	// Units is how many statements (Go) or lines (LCOV) the block holds.
	Units int
	Hit   bool
}

// Profile is a parsed coverage report.
type Profile struct {
	// Unit is "statements" (Go) or "lines" (LCOV).
	Unit   string
	Blocks []Block
}

// ParseCoverage reads a Go coverage profile (`go test -coverprofile`) or an
// LCOV tracefile, whichever the content is.
func ParseCoverage(r io.Reader) (Profile, error) {
	br := bufio.NewReader(io.LimitReader(r, MaxReportBytes+1))
	head, err := br.Peek(5)
	if err != nil && !errors.Is(err, io.EOF) {
		return Profile{}, err
	}
	if bytes.HasPrefix(head, []byte("mode:")) {
		return parseGoCover(br)
	}
	return parseLCOV(br)
}

// parseGoCover reads lines "file.go:12.3,15.4 2 1": the block, its
// statements and its hit count. Merged profiles repeat blocks; any hit wins.
func parseGoCover(r io.Reader) (Profile, error) {
	p := Profile{Unit: "statements"}
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	read := 0
	for sc.Scan() {
		line := sc.Text()
		read += len(line) + 1
		if read > MaxReportBytes {
			return Profile{}, ErrTooLarge
		}
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		file, rest, ok := cutLast(line, ":")
		if !ok {
			return Profile{}, fmt.Errorf("go coverage: malformed line %q", clip(line))
		}
		fields := strings.Fields(rest)
		if len(fields) != 3 {
			return Profile{}, fmt.Errorf("go coverage: malformed line %q", clip(line))
		}
		from, to, ok := strings.Cut(fields[0], ",")
		if !ok {
			return Profile{}, fmt.Errorf("go coverage: malformed range %q", clip(fields[0]))
		}
		start, err1 := lineOf(from)
		end, err2 := lineOf(to)
		stmts, err3 := strconv.Atoi(fields[1])
		count, err4 := strconv.ParseInt(fields[2], 10, 64)
		if err := errors.Join(err1, err2, err3, err4); err != nil || stmts < 0 || count < 0 || end < start {
			return Profile{}, fmt.Errorf("go coverage: malformed line %q", clip(line))
		}
		file, err := cleanPath(file)
		if err != nil {
			return Profile{}, err
		}
		key := file + "\x00" + fields[0]
		if i, ok := seen[key]; ok {
			p.Blocks[i].Hit = p.Blocks[i].Hit || count > 0
			continue
		}
		if len(p.Blocks) == MaxBlocks {
			return Profile{}, ErrTooLarge
		}
		seen[key] = len(p.Blocks)
		p.Blocks = append(p.Blocks, Block{File: file, StartLine: start, EndLine: end, Units: stmts, Hit: count > 0})
	}
	return p, sc.Err()
}

// lineOf reads the line of "12.3" (line.column).
func lineOf(pos string) (int, error) {
	l, _, _ := strings.Cut(pos, ".")
	n, err := strconv.Atoi(l)
	if err == nil && n < 1 {
		err = errors.New("line before 1")
	}
	return n, err
}

// parseLCOV reads SF:<file>, DA:<line>,<hits> and end_of_record; the
// other records (functions, branches) are ignored.
func parseLCOV(r io.Reader) (Profile, error) {
	p := Profile{Unit: "lines"}
	seen := map[string]int{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	file, read := "", 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		read += len(line) + 1
		if read > MaxReportBytes {
			return Profile{}, ErrTooLarge
		}
		switch {
		case strings.HasPrefix(line, "SF:"):
			f, err := cleanPath(strings.TrimPrefix(line, "SF:"))
			if err != nil {
				return Profile{}, err
			}
			file = f
		case strings.HasPrefix(line, "DA:"):
			if file == "" {
				return Profile{}, errors.New("lcov: DA record outside a file")
			}
			parts := strings.Split(strings.TrimPrefix(line, "DA:"), ",")
			if len(parts) < 2 {
				return Profile{}, fmt.Errorf("lcov: malformed record %q", clip(line))
			}
			n, err1 := strconv.Atoi(parts[0])
			hits, err2 := strconv.ParseInt(parts[1], 10, 64)
			if err := errors.Join(err1, err2); err != nil || n < 1 || hits < 0 {
				return Profile{}, fmt.Errorf("lcov: malformed record %q", clip(line))
			}
			key := file + "\x00" + parts[0]
			if i, ok := seen[key]; ok {
				p.Blocks[i].Hit = p.Blocks[i].Hit || hits > 0
				continue
			}
			if len(p.Blocks) == MaxBlocks {
				return Profile{}, ErrTooLarge
			}
			seen[key] = len(p.Blocks)
			p.Blocks = append(p.Blocks, Block{File: file, StartLine: n, EndLine: n, Units: 1, Hit: hits > 0})
		case line == "end_of_record":
			file = ""
		}
	}
	if err := sc.Err(); err != nil {
		return Profile{}, err
	}
	if len(p.Blocks) == 0 {
		return Profile{}, errors.New("not a Go coverage profile or LCOV tracefile, or it measured nothing")
	}
	return p, nil
}

// Outcome is how a test case ended.
type Outcome string

// Outcomes of a test case.
const (
	Passed  Outcome = "passed"
	Failed  Outcome = "failed"
	Skipped Outcome = "skipped"
)

// TestCase is one <testcase> of a JUnit XML report.
type TestCase struct {
	// Suite is the classname: a Go package path, a Python module (and
	// class), or whatever the JavaScript reporter puts there.
	Suite   string
	Name    string
	File    string
	Outcome Outcome
	Millis  int
}

// ParseJUnit reads the test cases of a JUnit XML report (go-junit-report,
// gotestsum, pytest, jest-junit and most CI tools write it). encoding/xml
// expands no external entities.
func ParseJUnit(r io.Reader) ([]TestCase, error) {
	dec := xml.NewDecoder(io.LimitReader(r, MaxReportBytes))
	var out []TestCase
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("junit: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "testcase" {
			continue
		}
		var tc struct {
			Classname string    `xml:"classname,attr"`
			Name      string    `xml:"name,attr"`
			File      string    `xml:"file,attr"`
			Time      string    `xml:"time,attr"`
			Failure   *struct{} `xml:"failure"`
			Error     *struct{} `xml:"error"`
			Skipped   *struct{} `xml:"skipped"`
		}
		if err := dec.DecodeElement(&tc, &start); err != nil {
			return nil, fmt.Errorf("junit: %w", err)
		}
		if len(out) == MaxTestCases {
			return nil, ErrTooLarge
		}
		c := TestCase{Suite: clipTo(tc.Classname, maxNameLen), Name: clipTo(tc.Name, maxNameLen), Outcome: Passed}
		if f, err := cleanPath(tc.File); err == nil {
			c.File = f
		}
		switch {
		case tc.Failure != nil || tc.Error != nil:
			c.Outcome = Failed
		case tc.Skipped != nil:
			c.Outcome = Skipped
		}
		if secs, err := strconv.ParseFloat(tc.Time, 64); err == nil && secs >= 0 && secs < 1e7 {
			c.Millis = int(secs * 1000)
		}
		if c.Name != "" {
			out = append(out, c)
		}
	}
	return out, nil
}

// cleanPath turns a reported path into slash form without "./" or a
// leading "/"; mapping matches it against repository files by suffix.
func cleanPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || len(p) > maxPathLen || strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("invalid path %q", clip(p))
	}
	p = strings.TrimLeft(path.Clean("/"+p), "/")
	if p == "" || p == "." {
		return "", fmt.Errorf("invalid path %q", clip(p))
	}
	return p, nil
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

func clip(s string) string { return clipTo(s, 80) }

func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
