// Package asklocal is the ask_local tool: an agent hands a question and some
// files of its checkout to a local model and gets a short answer with
// file:line citations, instead of reading the files into its own (paid)
// context. The bridge on the developer's machine reads the files, the API
// asks the model routed to the local-only "local-answer" task, and the
// citations are checked against the lines that were sent, so the agent can
// verify what matters with narrow reads.
package asklocal

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Tool is the MCP tool name.
const Tool = "ask_local"

// Limits keep one call bounded on both sides.
const (
	MaxQuestion = 2000
	// MaxPaths is how many path arguments a call takes; directories expand
	// to their tracked files, at most MaxFiles in all.
	MaxPaths = 50
	MaxFiles = 200
	// MaxFileBytes skips larger files (generated code, data); MaxTotalBytes
	// bounds what one call sends to the model, split into batches the
	// model's context holds.
	MaxFileBytes  = 512 << 10
	MaxTotalBytes = 2 << 20
	// MaxAnswer bounds what goes back into the agent's context.
	MaxAnswer = 8000
)

// ErrInvalid marks arguments the agent can fix.
var ErrInvalid = errors.New("invalid ask_local arguments")

// In is the tool's input.
type In struct {
	Question string   `json:"question" jsonschema:"what you want to know about the files, in one or two sentences"`
	Paths    []string `json:"paths" jsonschema:"repository-relative files or directories (only files git tracks are read); add :start-end for a line range, e.g. internal/api/api.go:100-240"`
}

// File is one file, or a line range of it, as sent to the model. Text holds
// lines Start..End.
type File struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

// Request is what the bridge sends the API.
type Request struct {
	Question string `json:"question"`
	Files    []File `json:"files"`
}

// Citation is a range the answer cites that was among the lines sent.
type Citation struct {
	Path  string `json:"path"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// Answer is the tool's result.
type Answer struct {
	Answer string `json:"answer"`
	Model  string `json:"model"`
	// Citations are checked against the lines sent; Unverified counts the
	// ones that were not among them.
	Citations  []Citation `json:"citations"`
	Unverified int        `json:"unverified_citations,omitempty"`
	// Grounded is false when the answer cites nothing it was given; treat
	// it as a lead, not a fact.
	Grounded bool `json:"grounded"`
	Files    int  `json:"files"`
	Lines    int  `json:"lines"`
	Batches  int  `json:"batches"`
	// InputTokens is what the local model read in place of the agent.
	InputTokens int    `json:"input_tokens"`
	Note        string `json:"note,omitempty"`
}

var specRe = regexp.MustCompile(`^(.+?):(\d+)-(\d+)$`)

// ParseSpec splits "path" or "path:start-end". start and end are 0 for a
// whole file.
func ParseSpec(spec string) (path string, start, end int, err error) {
	spec = strings.TrimSpace(spec)
	m := specRe.FindStringSubmatch(spec)
	if m == nil {
		return spec, 0, 0, nil
	}
	start, _ = strconv.Atoi(m[2])
	end, _ = strconv.Atoi(m[3])
	if start < 1 || end < start {
		return "", 0, 0, fmt.Errorf("%w: line range in %q must be start-end with 1 <= start <= end", ErrInvalid, spec)
	}
	return m[1], start, end, nil
}

// Validate checks a request against the limits.
func (r Request) Validate() error {
	q := strings.TrimSpace(r.Question)
	switch {
	case q == "":
		return fmt.Errorf("%w: question is empty", ErrInvalid)
	case utf8.RuneCountInString(q) > MaxQuestion:
		return fmt.Errorf("%w: question is longer than %d characters", ErrInvalid, MaxQuestion)
	case len(r.Files) == 0:
		return fmt.Errorf("%w: no files to read", ErrInvalid)
	case len(r.Files) > MaxFiles:
		return fmt.Errorf("%w: more than %d files", ErrInvalid, MaxFiles)
	}
	total := 0
	for _, f := range r.Files {
		total += len(f.Text)
		if f.Path == "" || strings.HasPrefix(f.Path, "/") || strings.Contains(f.Path, "..") || f.Start < 1 || f.End < f.Start || !utf8.ValidString(f.Text) {
			return fmt.Errorf("%w: file %q", ErrInvalid, f.Path)
		}
	}
	if total > MaxTotalBytes {
		return fmt.Errorf("%w: %d bytes of files, more than %d; ask about fewer files or line ranges", ErrInvalid, total, MaxTotalBytes)
	}
	return nil
}

// Numbered renders f with its line numbers, so the model can cite them.
func Numbered(f File) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== %s (lines %d-%d) ===\n", f.Path, f.Start, f.End)
	n := f.Start
	for line := range strings.Lines(f.Text) {
		fmt.Fprintf(&b, "%d| %s", n, line)
		if !strings.HasSuffix(line, "\n") {
			b.WriteByte('\n')
		}
		n++
	}
	return b.String()
}

// citeRe matches path:line and path:start-end. The path has an extension or
// a slash, so plain numbers and times are not taken for citations.
var citeRe = regexp.MustCompile(`([A-Za-z0-9_.\-/]*[./][A-Za-z0-9_\-]+):(\d+)(?:-(\d+))?`)

// Cite returns the citations in answer that fall inside the files sent and
// how many did not. Models shorten paths after naming them once (llm.go:12
// for internal/llm/llm.go), so a path that ends exactly one sent file's
// path, at a slash, counts as that file.
func Cite(answer string, files []File) ([]Citation, int) {
	var out []Citation
	seen := map[Citation]bool{}
	bad := 0
	for _, m := range citeRe.FindAllStringSubmatch(answer, -1) {
		start, _ := strconv.Atoi(m[2])
		end := start
		if m[3] != "" {
			end, _ = strconv.Atoi(m[3])
		}
		c := Citation{Path: resolve(strings.TrimPrefix(m[1], "./"), files), Start: start, End: end}
		if seen[c] {
			continue
		}
		seen[c] = true
		if within(c, files) {
			out = append(out, c)
		} else {
			bad++
		}
	}
	return out, bad
}

// resolve returns the sent path p names: itself, or the one path it ends.
func resolve(p string, files []File) string {
	match := ""
	for _, f := range files {
		switch {
		case f.Path == p:
			return p
		case strings.HasSuffix(f.Path, "/"+p) && match != f.Path:
			if match != "" {
				return p // ambiguous: two files end this way
			}
			match = f.Path
		}
	}
	if match == "" {
		return p
	}
	return match
}

func within(c Citation, files []File) bool {
	if c.End < c.Start {
		return false
	}
	for _, f := range files {
		if f.Path == c.Path && c.Start >= f.Start && c.End <= f.End {
			return true
		}
	}
	return false
}
