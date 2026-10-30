// Package prose keeps the text CodeSynapse's own models write terse: a
// constant style block for the system prompts of its generation tasks, and a
// bounded lint that finds greetings and filler phrases in what came back.
//
// The lint measures; it never rejects or rewrites model output. Tests use it
// to fail fixtures and golden outputs that contain filler, and the tasks log
// how many phrases an answer had, never the text.
package prose

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NoFiller is the part of the style block that suits every task, including
// those that must answer in a fixed structure such as JSON.
const NoFiller = `Style: write no greetings, sign-offs or offers of further help, and no filler phrases such as "Note that", "In order to", "As you can see", "It's worth noting", "Let's" or "Great question".`

// Style is the block appended to the system prompts of the Markdown
// generation tasks. It is constant, so provider prompt caches keep matching,
// and it yields to the format each task asks for.
const Style = NoFiller + ` Use direct, imperative phrasing ("Returns the user", not "This function will return the user"). When you describe one function, method or type, give one sentence on what it does, then a table of its parameters and return values, then one minimal example. The sections, format and length limits above take precedence over this style.`

// Kinds of phrases the lint finds.
const (
	Filler   = "filler"
	Greeting = "greeting"
)

// MaxLintBytes bounds how much of a text is examined.
const MaxLintBytes = 1 << 20

// phrase is lower-case with a plain apostrophe; the text is normalised the
// same way before matching.
type phrase struct{ text, kind string }

var phrases = []phrase{
	{"note that", Filler},
	{"please note", Filler},
	{"in order to", Filler},
	{"as you can see", Filler},
	{"it's worth noting", Filler},
	{"it is worth noting", Filler},
	{"it's worth mentioning", Filler},
	{"it's important to note", Filler},
	{"it is important to note", Filler},
	{"let's", Filler},
	{"let us", Filler},
	{"great question", Filler},
	{"good question", Filler},
	{"needless to say", Filler},
	{"as mentioned earlier", Filler},
	{"as previously mentioned", Filler},
	{"at the end of the day", Filler},
	{"first and foremost", Filler},
	{"dive into", Filler},
	{"delve into", Filler},
	{"hello!", Greeting},
	{"hello,", Greeting},
	{"hi there", Greeting},
	{"hey there", Greeting},
	{"certainly!", Greeting},
	{"sure!", Greeting},
	{"of course!", Greeting},
	{"absolutely!", Greeting},
	{"i hope this helps", Greeting},
	{"hope this helps", Greeting},
	{"happy coding", Greeting},
	{"feel free to", Greeting},
	{"let me know", Greeting},
	{"don't hesitate", Greeting},
	{"thanks for", Greeting},
	{"thank you for", Greeting},
	{"best regards", Greeting},
	{"kind regards", Greeting},
}

// Hit is one phrase found in a text and how often.
type Hit struct {
	Phrase string
	Kind   string
	Count  int
}

// Lint returns the phrases found in text, in list order, matched without
// regard to case and only at word boundaries ("denote that" is not "note
// that"). Only the first MaxLintBytes are examined.
func Lint(text string) []Hit {
	if len(text) > MaxLintBytes {
		text = text[:MaxLintBytes]
	}
	s := strings.ReplaceAll(strings.ToLower(text), "’", "'")
	var hits []Hit
	for _, p := range phrases {
		if n := count(s, p.text); n > 0 {
			hits = append(hits, Hit{Phrase: p.text, Kind: p.kind, Count: n})
		}
	}
	return hits
}

// Counts returns how many filler phrases and greetings text has.
func Counts(text string) (filler, greeting int) {
	for _, h := range Lint(text) {
		if h.Kind == Greeting {
			greeting += h.Count
		} else {
			filler += h.Count
		}
	}
	return filler, greeting
}

// Check returns an error naming the phrases in text, or nil. Tests use it on
// fixtures and golden outputs.
func Check(text string) error {
	hits := Lint(text)
	if len(hits) == 0 {
		return nil
	}
	found := make([]string, len(hits))
	for i, h := range hits {
		found[i] = fmt.Sprintf("%q (%s, %d)", h.Phrase, h.Kind, h.Count)
	}
	return fmt.Errorf("text has greetings or filler: %s", strings.Join(found, ", "))
}

func count(s, p string) int {
	first, _ := utf8.DecodeRuneInString(p)
	last, _ := utf8.DecodeLastRuneInString(p)
	n := 0
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], p)
		if j < 0 {
			break
		}
		start, end := i+j, i+j+len(p)
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (start == 0 || !word(first) || !word(before)) && (end == len(s) || !word(last) || !word(after)) {
			n++
			i = end
			continue
		}
		i = start + 1
	}
	return n
}

func word(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
