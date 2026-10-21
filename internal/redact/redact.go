// Package redact removes likely secrets from text before it is persisted.
//
// codesynapse never stores source bodies, but it does store short summaries
// (doc comments, doc sections, commit subjects). Those can contain pasted
// tokens or connection strings, and anything in the graph is later served to
// AI agents, so every summary passes through Text before it is written.
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Placeholder replaces every redacted span.
const Placeholder = "[REDACTED]"

type rule struct {
	re   *regexp.Regexp
	repl string
}

func r(expr string) rule { return rule{regexp.MustCompile(expr), Placeholder} }

var rules = []rule{
	// PEM private key blocks.
	r(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	// AWS access key IDs.
	r(`\b(AKIA|ASIA)[0-9A-Z]{16}\b`),
	// GitHub tokens.
	r(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`),
	r(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
	// Slack tokens.
	r(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`),
	// Anthropic / OpenAI style API keys.
	r(`\bsk-(ant-)?[A-Za-z0-9_-]{20,}\b`),
	// Google API keys.
	r(`\bAIza[0-9A-Za-z_-]{35}\b`),
	// JWTs.
	r(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	// Credentials embedded in URLs: scheme://user:pass@host.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@]+:)[^\s@/]+@`), "${1}" + Placeholder + "@"},
	// key=value / key: value assignments for secret-looking keys.
	{regexp.MustCompile(`(?i)\b((?:api[_-]?key|secret|token|passwd|password|pwd|auth)[a-z0-9_-]*\s*[:=]\s*)["']?[^\s"',;]{6,}["']?`), "${1}" + Placeholder},
}

// Text returns s with likely secrets replaced by Placeholder.
func Text(s string) string {
	for _, rl := range rules {
		s = rl.re.ReplaceAllString(s, rl.repl)
	}
	return s
}

// Summary redacts s, collapses whitespace and truncates it to at most limit
// runes, appending an ellipsis when truncated. Short summaries are what make
// graph answers cheaper than reading files, so callers should keep limit small.
func Summary(s string, limit int) string {
	s = strings.Join(strings.Fields(Text(s)), " ")
	if limit <= 0 || utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:limit-1])) + "…"
}

// FirstSentence returns the first sentence of s (up to ". " or a blank line).
func FirstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	return s
}
