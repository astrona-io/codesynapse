package asklocal

import (
	"regexp"
	"strings"
)

// The pre-brief reads files and calls the local model for every prompt that
// passes this gate, so it must stay quiet when a prompt is not about code.
// A real session (PLAN item 18, 2026-10-28) briefed "start more agents", an
// instruction to the assistant: the graph matched functions named Start, the
// model found nothing relevant, and the agent got a brief that only said so.

var (
	// codeToken is something only code has: `quoted`, camelCase or
	// CamelCase with an inner capital, snake_case, a dotted name (not a
	// version: "v0.0.21" stays prose), a call or a path.
	codeToken = regexp.MustCompile("`[^`]+`" +
		`|\b[a-z]+[A-Z][A-Za-z0-9]*|\b[A-Z][a-z0-9]+[A-Z][A-Za-z0-9]*` +
		`|\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]+` +
		`|\b[A-Za-z_][A-Za-z0-9_]*\.[A-Za-z_][A-Za-z0-9_]*` +
		`|\b[A-Za-z_][A-Za-z0-9_]*\(\)` +
		`|\b[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+`)
	// capitalName is a capitalised word inside a sentence ("make Handler
	// validate"): often a type, but also a typo'd product name ("new CLI
	// Realase"), so it counts only when the prompt is not a work instruction.
	capitalName = regexp.MustCompile(`[A-Za-z0-9,;]\s+[A-Z][a-z0-9]+\b`)
	// workInstruction tells the assistant how to work rather than what to
	// change: "remember to rebuild …", "you are allowed to …", "you can merge
	// … when it passes", "from now on …". Misspellings are how people type.
	workInstruction = regexp.MustCompile(`(?i)\b(rem(em)?e?ber|from now on|you('re| are| r) allowed|you can|you may|feel free|no need to ask|every ?time you)\b`)
	// errorText is how a failure shows up in a prompt.
	errorText = regexp.MustCompile(`(?i)\b(error|errors|panic|panics|fails?|failing|failed|failure|crash(es|ed)?|exception|stack ?trace|nil pointer|undefined|timeout|deadlock|race|[45]\d\d)\b`)
	// taskVerb asks for work on code, or asks why or how it behaves.
	taskVerb = regexp.MustCompile(`(?i)\b(fix|fixes|implement|add|refactor|rename|remove|delete|debug|test|write|change|update|move|extract|split|merge|review|explain|why|how|where|what)\b`)
	// codeNoun is a part of a code base a task verb can aim at.
	codeNoun = regexp.MustCompile(`(?i)\b(functions?|methods?|types?|structs?|interfaces?|hooks?|handlers?|endpoints?|routes?|api|tests?|files?|packages?|modules?|pages?|components?|quer(y|ies)|schemas?|fields?|flags?|config|settings|builds?|lint|ci|bugs?|counters?|metrics?|logs?|logging|jobs?|workers?|services?|servers?|clients?|cli|commands?|charts?|migrations?|index|graph|stores?|cache|commits?|branch(es)?|diffs?|parsers?|detectors?|rules?|findings?|tokens?|sessions?|requests?|responses?|errors?|dashboard|ui|tools?|briefs?)\b`)
)

// briefWords is a prompt long enough to describe a task without naming code.
const briefWords = 20

// WorthBriefing reports whether a prompt looks like a task on code: it names
// code (an identifier, path or file), quotes an error, aims a task verb at a
// part of the code base, or is long enough to describe one. Short
// conversational turns ("start more agents", "new release installed, we can
// continue") are not briefed.
//
// A work instruction ("remember to rebuild the apps …, you are allowed to do
// that every time") is not briefed unless it names code or quotes an error,
// however long it is.
func WorthBriefing(prompt string) bool {
	p := strings.TrimSpace(prompt)
	switch {
	case p == "", HarnessTurn(p):
		return false
	case codeToken.MatchString(p), errorText.MatchString(p):
		return true
	case workInstruction.MatchString(p):
		return false
	case capitalName.MatchString(p), aimsAtCode(p):
		return true
	}
	return len(strings.Fields(p)) >= briefWords
}

// aimsAtCode reports whether a task verb and a code noun are two different
// words: "test" is both, and "so I can test the features" names no code.
func aimsAtCode(p string) bool {
	verbs, nouns := taskVerb.FindAllStringIndex(p, -1), codeNoun.FindAllStringIndex(p, -1)
	for _, v := range verbs {
		for _, n := range nouns {
			if v[0] != n[0] {
				return true
			}
		}
	}
	return false
}

// harnessPrefixes open the turns Claude Code starts on its own rather than
// the developer typing them: a background task's notification, another
// session's or a subagent's message, a reminder or a system notification.
// UserPromptSubmit fires for them with the text in "prompt" and no field that
// says where it came from, so the text is all there is to go on. A real
// session (PLAN item 18, 2026-10-28) briefed a subagent's hand-back: long and
// full of identifiers, it matched twelve unrelated helpers by common words.
var harnessPrefixes = []string{
	"<task-notification>",
	"<agent-message",
	"<system-reminder>",
	"[system notification",
	"another claude session sent a message",
}

// HarnessTurn reports whether a prompt is a turn the harness injected, not
// one the developer wrote.
func HarnessTurn(prompt string) bool {
	p := strings.ToLower(strings.TrimSpace(prompt))
	for _, h := range harnessPrefixes {
		if strings.HasPrefix(p, h) {
			return true
		}
	}
	return false
}

// NothingRelevant reports whether a brief only says the files do not bear
// on the request ("Nothing relevant in the given files. The developer
// should clarify..."): the agent learns nothing from it, so it is dropped.
func NothingRelevant(answer string) bool {
	a := strings.ToLower(strings.TrimLeft(answer, " \t\r\n*#_>-"))
	for _, p := range []string{"nothing relevant", "not in the given files", "no relevant code", "none of the provided files", "none of the given files"} {
		if strings.HasPrefix(a, p) {
			return true
		}
	}
	return strings.Contains(a, "nothing relevant in the given files") || strings.Contains(a, "no relevant code for the request")
}
