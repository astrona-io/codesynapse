package redact

import (
	"regexp"
	"strings"
)

// Credentials with a provider-specific shape, for scans that must find
// real secrets rather than mask anything that might be one (the git
// history scan). Generic key=value assignments are left out: in history
// they are mostly test fixtures and examples.

// Credential is one match: its kind and the matched text. The text is for
// comparing and hashing in memory only; never store or log it.
type Credential struct {
	Kind  string
	Value string
}

var credentials = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"AWS access key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`)},
	{"GitHub token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`)},
	{"Slack token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)},
	{"OpenAI or Anthropic API key", regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{20,}\b`)},
	{"Google API key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
}

// PrivateKeyHeader starts a PEM private key block.
var PrivateKeyHeader = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)

var (
	pemHeaderLine = regexp.MustCompile(`^\s*-----BEGIN [A-Z ]*PRIVATE KEY-----\s*$`)
	pemBodyLine   = regexp.MustCompile(`^[A-Za-z0-9+/=]{40,}$`)
)

// PrivateKeyStart reports a line that is only a PEM private key header, as
// in a key file (not a header quoted inside code or prose).
func PrivateKeyStart(line string) bool { return pemHeaderLine.MatchString(line) }

// PrivateKeyBody returns the line after a header as a credential when it
// is base64 key material.
func PrivateKeyBody(line string) (Credential, bool) {
	v := strings.TrimSpace(line)
	if !pemBodyLine.MatchString(v) || placeholderCredential(v) {
		return Credential{}, false
	}
	return Credential{Kind: "Private key", Value: v}, true
}

// CredentialPickaxe is a POSIX extended regular expression for the same
// shapes, for git log -G to select candidate commits cheaply. It holds
// patterns only, never a value.
//
//nolint:gosec // G101: a pattern to search for, not a credential
const CredentialPickaxe = `(AKIA|ASIA)[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22}|xox[abprs]-[A-Za-z0-9-]{10}|sk-[A-Za-z0-9_-]{20}|AIza[0-9A-Za-z_-]{35}|-----BEGIN [A-Z ]*PRIVATE KEY-----`

// Credentials finds provider-shaped credentials in s, leaving out obvious
// placeholders (documentation examples, repeated characters).
func Credentials(s string) []Credential {
	var out []Credential
	for _, c := range credentials {
		for _, m := range c.re.FindAllString(s, 8) {
			if !placeholderCredential(m) {
				out = append(out, Credential{Kind: c.kind, Value: m})
			}
		}
	}
	return out
}

// placeholderCredential reports documentation examples (AKIAIOSFODNN7EXAMPLE)
// and filler (ghp_xxxx…): too few distinct characters to be random.
func placeholderCredential(v string) bool {
	upper := strings.ToUpper(v)
	if strings.Contains(upper, "EXAMPLE") || strings.Contains(upper, "XXXXXX") || strings.Contains(upper, "REDACTED") {
		return true
	}
	distinct := map[rune]bool{}
	for _, r := range v {
		distinct[r] = true
	}
	return len(distinct) < 10
}
