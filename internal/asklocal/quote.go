package asklocal

import (
	"path"
	"regexp"
	"strings"
	"unicode"
)

// A real session (PLAN item 18, case g, 2026-10-29) quoted a hint of the
// Models statistics page and asked for it to become an info icon shown on
// mouse over. The graph holds no literal text (no source bodies), so the plan
// matched the generic word "stats" to eleven Go Stats structs, and the brief
// said the quoted phrase appears in none of them. A quote names the place
// better than any word in it, and UI words point at the front end.

const (
	// MinQuoteWords is the shortest quote worth looking up as literal text;
	// shorter ones ("the", "Stats") are names, not sentences.
	MinQuoteWords = 3
	// MaxQuotes bounds how many quoted phrases one prompt looks up.
	MaxQuotes = 3
	// maxQuoteBytes caps one quoted phrase.
	maxQuoteBytes = 400
	// MaxFragments bounds the literal fragments one prompt searches.
	MaxFragments = 12
)

// quoteRe finds text in straight or curly double quotes, or in guillemets.
// Straight single quotes are left out: they are apostrophes far more often.
var quoteRe = regexp.MustCompile(`"([^"\n]+)"|“([^”\n]+)”|„([^“”\n]+)[“”]|«([^»\n]+)»|‘([^’\n]+)’`)

// QuotedPhrases returns the phrases a prompt quotes that have at least
// MinQuoteWords words, at most MaxQuotes of them, each capped in size.
func QuotedPhrases(prompt string) []string {
	var out []string
	for _, m := range quoteRe.FindAllStringSubmatch(prompt, -1) {
		var p string
		for _, g := range m[1:] {
			if g != "" {
				p = strings.TrimSpace(g)
				break
			}
		}
		if len(strings.Fields(p)) < MinQuoteWords || len(p) > maxQuoteBytes {
			continue
		}
		out = append(out, p)
		if len(out) == MaxQuotes {
			break
		}
	}
	return out
}

// QuoteFragments splits a quoted phrase into lowercase runs of at least
// MinQuoteWords words, cut at every character that is not a letter, digit,
// space or hyphen. The code holds the same words with its own punctuation
// ("CodeSynapse\'s", "&#39;", a line break after a comma), so fragments match
// where the whole phrase would not. A single letter left at either end of a
// run (the "s" of a possessive) is dropped.
func QuoteFragments(phrase string) []string {
	runs := strings.FieldsFunc(strings.ToLower(phrase), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' && r != '-'
	})
	var out []string
	for _, r := range runs {
		w := strings.Fields(r)
		for len(w) > 0 && len([]rune(w[0])) == 1 {
			w = w[1:]
		}
		for len(w) > 0 && len([]rune(w[len(w)-1])) == 1 {
			w = w[:len(w)-1]
		}
		if len(w) >= MinQuoteWords {
			out = append(out, strings.Join(w, " "))
		}
	}
	return out
}

// uiWords are what a prompt about the user interface talks about.
var uiWords = regexp.MustCompile(`(?i)\b(icons?|mouse[- ]?over|on hover|hover(s|ing)?|tool ?tips?|buttons?|pages?|dashboard|modals?|dialogs?|popovers?|sidebar|navbar|menus?|tabs?|badges?|layout|css|styl(e|es|ing)|ui|ux|clicks?|clicked|screen|theme|dark mode|light mode)\b`)

// AboutUI reports whether a prompt is about the user interface.
func AboutUI(prompt string) bool {
	return uiWords.MatchString(prompt)
}

// FrontEnd reports whether a repository path is front-end code: a component
// or style file, or a script under a web app, its pages or its components.
func FrontEnd(p string) bool {
	switch path.Ext(p) {
	case ".vue", ".svelte", ".tsx", ".jsx", ".css", ".scss", ".sass", ".less", ".html":
		return true
	case ".ts", ".js", ".mjs":
		p = "/" + p
		for _, dir := range []string{"/web/", "/frontend/", "/pages/", "/components/", "/composables/", "/layouts/"} {
			if strings.Contains(p, dir) {
				return true
			}
		}
	}
	return false
}
