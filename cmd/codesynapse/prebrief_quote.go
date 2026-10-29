package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// Quoted text in a prompt names the place to work better than any word in
// it, but the graph holds no literal text (no source bodies). PLAN item 18,
// case g (2026-10-29): a prompt quoted a hint of the Models statistics page,
// the plan matched "stats" to eleven Go Stats structs, and the brief said the
// quote was in none of them. The pre-brief now looks quotes up in the
// checkout it already reads, with one bounded git grep.

// quoteMissedNote tells the agent the quoted text is in no file git tracks,
// rather than leaving it to name matches that only look related.
const quoteMissedNote = "CodeSynapse pre-brief: the text quoted in the prompt was not found as literal text (case-insensitive, punctuation ignored) in any file git tracks in this checkout. It may be built at run time or worded differently in the code: search for it before relying on any lead below, which come from name matches in the graph only."

const (
	// quoteContext is how many lines around a quote match the brief reads.
	quoteContext = 20
	// maxQuoteFiles bounds the files a quote selects.
	maxQuoteFiles = 3
	// maxQuoteLines bounds the matching lines read from git grep.
	maxQuoteLines = 2000
	// maxQuoteMatchesPerFile is git grep's -m: enough to place a quote that
	// wraps over a few lines.
	maxQuoteMatchesPerFile = "20"
	// maxRangesAfterMiss is what is left of the graph's plan when the
	// quoted text is nowhere: fewer ranges, not padding.
	maxRangesAfterMiss = 3
)

// briefRanges decides what the brief reads. Files that hold the prompt's
// quoted text come first, followed only by those of the graph's ranges that
// lie in the same files: the quote says where the work is. When a quote is
// in no file, the graph's plan is cut to its first few ranges and missed is
// true, so the brief says so instead of guessing.
func briefRanges(ctx context.Context, root, prompt string, plan []string) (ranges []string, missed bool) {
	quoted, searched := quoteRanges(ctx, root, asklocal.QuotedPhrases(prompt))
	if len(quoted) == 0 {
		if searched {
			return plan[:min(len(plan), maxRangesAfterMiss)], true
		}
		return plan, false
	}
	files := map[string]bool{}
	for _, r := range quoted {
		if p, _, _, err := asklocal.ParseSpec(r); err == nil {
			files[p] = true
		}
	}
	ranges = quoted
	for _, r := range plan {
		if p, _, _, err := asklocal.ParseSpec(r); err == nil && files[p] && !slices.Contains(ranges, r) {
			ranges = append(ranges, r)
		}
	}
	return ranges, false
}

// quoteRanges looks the quoted phrases up as fixed, case-insensitive strings
// in the files git tracks under root, and returns a range around the matches
// in each file that holds at least half of a phrase's words, best first.
// searched is false when no phrase had a fragment to look for or git failed.
// The phrases are git arguments only (no shell); they are never logged.
func quoteRanges(ctx context.Context, root string, phrases []string) (ranges []string, searched bool) {
	type phrase struct {
		frags []string
		words int
	}
	var ps []phrase
	args := []string{"grep", "-n", "-z", "-I", "-i", "-F", "--no-color", "--full-name", "-m", maxQuoteMatchesPerFile}
	nfrags := 0
	for _, p := range phrases {
		var ph phrase
		for _, f := range asklocal.QuoteFragments(p) {
			if nfrags == asklocal.MaxFragments {
				break
			}
			nfrags++
			ph.frags = append(ph.frags, f)
			ph.words += len(strings.Fields(f))
			args = append(args, "-e", f)
		}
		if len(ph.frags) > 0 {
			ps = append(ps, ph)
		}
	}
	if len(ps) == 0 {
		return nil, false
	}
	out, err := gitx.Run(ctx, root, append(args, "--")...)
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		return nil, true // git grep found nothing
	case err != nil:
		return nil, false // too many matches or no git: no claim either way
	}
	type match struct {
		lines []int
		frags map[int]map[string]bool // phrase -> its fragments seen
	}
	byFile := map[string]*match{}
	for i, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if i == maxQuoteLines {
			break
		}
		// -z: path NUL line NUL text
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			continue
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil || n < 1 {
			continue
		}
		text := strings.ToLower(strings.Join(strings.Fields(parts[2]), " "))
		for pi, ph := range ps {
			for _, f := range ph.frags {
				if !strings.Contains(text, f) {
					continue
				}
				m := byFile[parts[0]]
				if m == nil {
					m = &match{frags: map[int]map[string]bool{}}
					byFile[parts[0]] = m
				}
				if m.frags[pi] == nil {
					m.frags[pi] = map[string]bool{}
				}
				m.frags[pi][f] = true
				if !slices.Contains(m.lines, n) {
					m.lines = append(m.lines, n)
				}
			}
		}
	}
	type hit struct {
		path        string
		score       float64
		first, last int
	}
	var hits []hit
	for path, m := range byFile {
		best := 0.0
		for pi, seen := range m.frags {
			words := 0
			for f := range seen {
				words += len(strings.Fields(f))
			}
			// At least half the phrase: a common run ("at the input
			// price") alone is not the quote.
			if 2*words >= ps[pi].words {
				best = max(best, float64(words)/float64(ps[pi].words))
			}
		}
		if best == 0 {
			continue
		}
		slices.Sort(m.lines)
		first, last := m.lines[0], m.lines[len(m.lines)-1]
		if last-first > 4*quoteContext {
			last = first // scattered matches: read around the first
		}
		hits = append(hits, hit{path, best, first, last})
	}
	slices.SortFunc(hits, func(a, b hit) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.path, b.path))
	})
	for _, h := range hits[:min(len(hits), maxQuoteFiles)] {
		ranges = append(ranges, fmt.Sprintf("%s:%d-%d", h.path, max(1, h.first-quoteContext), h.last+quoteContext))
	}
	return ranges, true
}
