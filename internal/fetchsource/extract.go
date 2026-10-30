package fetchsource

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Tier 2 (docs/plans/TEXT_OPTIMIZATION_PLAN.md, pillar B): when the cleaned
// and pruned page is still long, the bridge asks the server's local-only
// source-extract task for the parts that answer the question. The page and
// the question are sent for that one request and never stored; the answer
// is model output and is checked here before the agent sees it.

// Tier 2 limits.
const (
	// ExtractThreshold is the tier-1 text length (characters) from which
	// the bridge asks for an extract; shorter text is returned as it is.
	ExtractThreshold = 6000
	// MaxExtractInput bounds the page text sent (bytes). A longer page is
	// returned as tier 1 gave it: an extract of part of a page could miss
	// the answer and look complete.
	MaxExtractInput = 96 << 10
	// MaxExtract caps the extract (characters).
	MaxExtract = 8000
	// maxSnippets bounds the code spans and blocks checked in one answer.
	maxSnippets = 512
)

// NotInPage is what the model answers when the page does not answer the
// question; the bridge then returns the tier-1 text.
const NotInPage = "NOT IN THE PAGE"

// droppedCode replaces a code span the page does not contain.
const droppedCode = "[code not in the page, dropped]"

// ExtractRequest is the body of POST /api/v1/repos/{repo}/source-extract.
type ExtractRequest struct {
	Question string `json:"question"`
	Text     string `json:"text"`
}

// Validate checks the request against the limits.
func (r ExtractRequest) Validate() error {
	q := strings.TrimSpace(r.Question)
	switch {
	case q == "":
		return fmt.Errorf("%w: question is empty", ErrInvalid)
	case utf8.RuneCountInString(q) > MaxQuestion:
		return fmt.Errorf("%w: question is longer than %d characters", ErrInvalid, MaxQuestion)
	case strings.TrimSpace(r.Text) == "":
		return fmt.Errorf("%w: text is empty", ErrInvalid)
	case len(r.Text) > MaxExtractInput:
		return fmt.Errorf("%w: text is longer than %d bytes", ErrInvalid, MaxExtractInput)
	case !utf8.ValidString(r.Text) || !utf8.ValidString(q):
		return fmt.Errorf("%w: text is not valid UTF-8", ErrInvalid)
	}
	return nil
}

// Extract is the answer of source-extract.
type Extract struct {
	// Found is false when the model said the page does not answer the
	// question; Text is then empty.
	Found       bool   `json:"found"`
	Text        string `json:"text"`
	Model       string `json:"model,omitempty"`
	InputTokens int    `json:"input_tokens,omitempty"`
	// Dropped counts code the model wrote that the page does not contain.
	Dropped  int      `json:"dropped,omitempty"`
	Problems []string `json:"problems,omitempty"`
}

// CheckSnippets drops every code block and inline code span of answer whose
// code does not appear in sent: a local model that misquotes or invents code
// must not hand it to the agent as the page's. A block is kept only when
// each of its non-blank lines, trimmed, occurs in sent. It returns the
// checked answer and how many snippets were dropped. Past maxSnippets the
// rest of the answer is cut, so the check stays bounded.
func CheckSnippets(answer, sent string) (string, int) {
	var out strings.Builder
	lines := strings.Split(answer, "\n")
	n, drop := 0, 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fence := fenceOf(line); fence != "" {
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), fence) {
				j++
			}
			n++
			if n > maxSnippets {
				break
			}
			if blockInPage(lines[i+1:min(j, len(lines))], sent) {
				for _, l := range lines[i:min(j+1, len(lines))] {
					out.WriteString(l + "\n")
				}
			} else {
				out.WriteString(droppedCode + "\n")
				drop++
			}
			i = j
			continue
		}
		var k int
		line, k = checkInline(line, sent, &n)
		drop += k
		out.WriteString(line + "\n")
		if n > maxSnippets {
			break
		}
	}
	return strings.TrimRight(out.String(), "\n"), drop
}

// fenceOf is the fence a line opens (``` or ~~~), or "".
func fenceOf(line string) string {
	t := strings.TrimSpace(line)
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, f) {
			return f
		}
	}
	return ""
}

func blockInPage(block []string, sent string) bool {
	for _, l := range block {
		if t := strings.TrimSpace(l); t != "" && !strings.Contains(sent, t) {
			return false
		}
	}
	return true
}

// checkInline replaces the inline code spans of line that sent lacks.
func checkInline(line, sent string, n *int) (string, int) {
	if !strings.Contains(line, "`") {
		return line, 0
	}
	var b strings.Builder
	drop := 0
	for {
		open := strings.IndexByte(line, '`')
		if open < 0 {
			break
		}
		end := strings.IndexByte(line[open+1:], '`')
		if end < 0 {
			break
		}
		code := line[open+1 : open+1+end]
		*n++
		b.WriteString(line[:open])
		if t := strings.TrimSpace(code); t == "" || strings.Contains(sent, t) {
			b.WriteString(line[open : open+end+2])
		} else {
			b.WriteString(droppedCode)
			drop++
		}
		line = line[open+end+2:]
		if *n > maxSnippets {
			return b.String(), drop
		}
	}
	b.WriteString(line)
	return b.String(), drop
}
