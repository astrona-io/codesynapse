package redact

import (
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
)

func TestText(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		leaking string
	}{
		{name: "plain text untouched", in: "Parse returns the AST.", want: "Parse returns the AST."},
		{name: "aws key", in: "use AKIAABCDEFGHIJKLMNOP here", leaking: "AKIAABCDEFGHIJKLMNOP"},
		{name: "github token", in: "token ghp_" + strings.Repeat("a", 36), leaking: "ghp_"},
		{name: "anthropic key", in: "sk-ant-" + strings.Repeat("x", 30), leaking: "sk-ant-"},
		{name: "url credentials", in: "dial neo4j://neo4j:hunter22@db:7687", want: "dial neo4j://neo4j:[REDACTED]@db:7687"},
		{name: "assignment keeps key", in: "password=supersecret1", want: "password=[REDACTED]"},
		{name: "pem block", in: "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----", want: "[REDACTED]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.in)
			if tt.want != "" {
				assert.Equal(t, got, tt.want)
			}
			if tt.leaking != "" {
				assert.NotContains(t, got, tt.leaking)
				assert.Contains(t, got, Placeholder)
			}
		})
	}
}

func TestSummary(t *testing.T) {
	assert.Equal(t, Summary("a  b\n c", 0), "a b c")
	assert.Equal(t, Summary("abcdefghij", 5), "abcd…")
	assert.Equal(t, Summary("short", 10), "short")
}

func TestFirstSentence(t *testing.T) {
	assert.Equal(t, FirstSentence("Foo does x. Then y."), "Foo does x.")
	assert.Equal(t, FirstSentence("Para one\n\nPara two"), "Para one")
}

func FuzzText(f *testing.F) {
	f.Add("password=abc123456")
	f.Add("https://u:p@h")
	f.Fuzz(func(_ *testing.T, s string) {
		_ = Summary(s, 50) // must not panic
	})
}
