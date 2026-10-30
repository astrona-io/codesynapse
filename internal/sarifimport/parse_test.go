package sarifimport

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sampleLog has two tools: one in the shape gitleaks writes (rules with
// help links, relative URIs, a secret in the message and a snippet that
// must never be kept) and one with absolute paths under a file base.
const sampleLog = `{
  "version": "2.1.0",
  "$schema": "https://json.schemastore.org/sarif-2.1.0.json",
  "runs": [
    {
      "tool": {"driver": {"name": "gitleaks", "semanticVersion": "8.21.2",
        "rules": [{"id": "aws-access-token", "helpUri": "https://github.com/gitleaks/gitleaks#aws",
          "shortDescription": {"text": "AWS access token"},
          "properties": {"security-severity": "9.1"}}]}},
      "results": [
        {"ruleId": "aws-access-token", "level": "error",
         "message": {"text": "AWS token found: AKIAIOSFODNN7EXAMPLE"},
         "locations": [{"physicalLocation": {"artifactLocation": {"uri": "./config/app.yaml"},
           "region": {"startLine": 4, "endLine": 2, "snippet": {"text": "aws_key = AKIAIOSFODNN7EXAMPLE"}}}}]},
        {"ruleId": "aws-access-token",
         "message": {"text": "AWS token found: AKIAIOSFODNN7EXAMPLE"},
         "locations": [{"physicalLocation": {"artifactLocation": {"uri": "./config/app.yaml"},
           "region": {"startLine": 4, "endLine": 2}}}]},
        {"ruleId": "aws-access-token", "message": {"text": "outside"},
         "locations": [{"physicalLocation": {"artifactLocation": {"uri": "../../etc/passwd"}}}]},
        {"ruleId": "aws-access-token", "kind": "pass", "message": {"text": "fine"},
         "locations": [{"physicalLocation": {"artifactLocation": {"uri": "a.go"}}}]}
      ]
    },
    {
      "tool": {"driver": {"name": "Semgrep OSS", "version": "1.90.0",
        "rules": [{"id": "go.lang.security.audit.sqli", "defaultConfiguration": {"level": "note"},
          "helpUri": "javascript:alert(1)",
          "messageStrings": {"default": {"text": "query built from {0}"}}}]}},
      "originalUriBaseIds": {"SRCROOT": {"uri": "file:///home/runner/work/app/app/"}},
      "artifacts": [{"location": {"uri": "file:///home/runner/work/app/app/internal/db/q.go"}}],
      "results": [
        {"ruleIndex": 0, "message": {"id": "default", "arguments": ["user input"]},
         "locations": [{"physicalLocation": {"artifactLocation": {"index": 0}, "region": {"startLine": 12}}}],
         "suppressions": [{"kind": "inSource", "status": "accepted"}]},
        {"ruleId": "go.lang.security.audit.sqli", "message": {"text": "elsewhere"},
         "locations": [{"physicalLocation": {"artifactLocation": {"uri": "file:///opt/build/src/cmd/main.go"}}}]},
        {"ruleId": "go.lang.security.audit.sqli", "message": {"text": "web"},
         "locations": [{"physicalLocation": {"artifactLocation": {"uri": "https://example.com/x.go"}}}]}
      ]
    }
  ]
}`

func TestParse(t *testing.T) {
	runs, err := Parse(strings.NewReader(sampleLog))
	require.NoError(t, err)
	require.Equal(t, len(runs), 2)

	gl := runs[0]
	assert.Equal(t, gl.Tool, "gitleaks")
	assert.Equal(t, gl.Version, "8.21.2")
	require.Equal(t, len(gl.Findings), 1, "the duplicate result is kept once")
	assert.Equal(t, gl.Dropped, 2, "a path outside the repository and a pass")
	f := gl.Findings[0]
	assert.Equal(t, f.RuleID, "aws-access-token")
	assert.Equal(t, f.Severity, "critical", "security-severity 9.1 wins over the level")
	assert.Equal(t, f.Level, "error")
	assert.Equal(t, f.File, "config/app.yaml")
	assert.Equal(t, f.StartLine, 4)
	assert.Equal(t, f.EndLine, 4, "an end before the start is the start")
	assert.False(t, strings.Contains(f.Message, "AKIA"), "the secret is redacted: "+f.Message)
	assert.Equal(t, f.HelpURI, "https://github.com/gitleaks/gitleaks#aws")
	assert.Equal(t, len(f.Fingerprint), 24)

	sg := runs[1]
	assert.Equal(t, sg.Tool, "Semgrep OSS")
	assert.Equal(t, sg.Version, "1.90.0")
	assert.Equal(t, sg.Dropped, 1, "an https location")
	require.Equal(t, len(sg.Findings), 2)
	q := sg.Findings[0]
	assert.Equal(t, q.File, "internal/db/q.go", "made relative to SRCROOT")
	assert.Equal(t, q.StartLine, 12)
	assert.Equal(t, q.Level, "note", "the rule's default level")
	assert.Equal(t, q.Severity, "low")
	assert.Equal(t, q.Message, "query built from user input")
	assert.Equal(t, q.HelpURI, "", "only https links")
	assert.True(t, q.Suppressed)
	other := sg.Findings[1]
	assert.Equal(t, other.File, "", "outside every base: placed later")
	assert.Equal(t, other.abs, "/opt/build/src/cmd/main.go")
}

func TestParseRefuses(t *testing.T) {
	tests := []struct {
		name, log string
		want      error
	}{
		{"not json", "<xml/>", ErrInvalid},
		{"version", `{"version":"2.0.0","runs":[]}`, ErrInvalid},
		{"no runs", `{"version":"2.1.0","runs":[]}`, ErrInvalid},
		{"no tool name", `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"  "}},"results":[]}]}`, ErrInvalid},
		{"too many runs", `{"version":"2.1.0","runs":[` + strings.Repeat(`{"tool":{"driver":{"name":"x"}}},`, MaxRuns) + `{"tool":{"driver":{"name":"x"}}}]}`, ErrTooLarge},
		{"too large", `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"` + strings.Repeat("a", MaxLogBytes) + `"}}}]}`, ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.log))
			assert.True(t, errors.Is(err, tt.want), fmt.Sprint(err))
		})
	}
}

func TestSeverity(t *testing.T) {
	tests := []struct {
		level  string
		score  float64
		scored bool
		want   string
	}{
		{"error", 0, false, "high"},
		{"warning", 0, false, "medium"},
		{"note", 0, false, "low"},
		{"none", 0, false, "info"},
		{"note", 9.8, true, "critical"},
		{"note", 7, true, "high"},
		{"error", 4, true, "medium"},
		{"error", 0.1, true, "low"},
		{"error", 0, true, "info"},
	}
	for _, tt := range tests {
		assert.Equal(t, Severity(tt.level, tt.score, tt.scored), tt.want, fmt.Sprint(tt))
	}
}

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{"a/b.go": "a/b.go", "./a/../b.go": "b.go", "../x": "", "/etc/x": "", "a/\x00b": "", ".": "", "C:/x": ""} {
		got, ok := CleanPath(in)
		assert.Equal(t, got, want, in)
		assert.Equal(t, ok, want != "", in)
	}
}

type fakeFiles map[string]bool

func (f fakeFiles) HasFile(_ context.Context, file string) (bool, error) { return f[file], nil }

func TestPlace(t *testing.T) {
	run := Run{Tool: "x", Findings: []Finding{
		{RuleID: "r", File: "kept.go"},
		{RuleID: "r", abs: "/opt/build/src/cmd/main.go"},
		{RuleID: "r", abs: "/opt/build/src/internal/a.go"},
		{RuleID: "r", abs: "/elsewhere/nothing.go"},
	}}
	unmatched, err := Place(context.Background(), fakeFiles{"cmd/main.go": true, "internal/a.go": true}, &run)
	require.NoError(t, err)
	assert.Equal(t, unmatched, 1)
	require.Equal(t, len(run.Findings), 3)
	assert.Equal(t, run.Findings[1].File, "cmd/main.go")
	assert.Equal(t, run.Findings[2].File, "internal/a.go")
}

func TestSetWith(t *testing.T) {
	s := Set{}.With([]StoredRun{{Run: Run{Tool: "zizmor"}, Commit: "a"}, {Run: Run{Tool: "Gitleaks"}, Commit: "a"}})
	s = s.With([]StoredRun{{Run: Run{Tool: "gitleaks", Findings: []Finding{{RuleID: "r"}}}, Commit: "b"}})
	require.Equal(t, len(s.Runs), 2)
	assert.Equal(t, s.Runs[0].Tool, "gitleaks", "replaced, case-insensitive")
	assert.Equal(t, s.Runs[0].Commit, "b")
	assert.Equal(t, s.Total(), 1)
	s, ok := s.Without("ZIZMOR")
	assert.True(t, ok)
	assert.Equal(t, len(s.Runs), 1)
	_, ok = s.Without("zizmor")
	assert.False(t, ok)
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(sampleLog))
	f.Add([]byte(`{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[{"ruleId":"r","locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///C:/w/../../x.go"}}}]}]}]}`))
	f.Add([]byte(`{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"t"}},"results":[{"ruleIndex":-1,"locations":[{"physicalLocation":{"artifactLocation":{"uri":"%2e%2e/x"}}}]}]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		runs, err := Parse(strings.NewReader(string(b)))
		if err != nil {
			return
		}
		for _, r := range runs {
			if r.Tool == "" || len([]rune(r.Tool)) > maxNameRunes {
				t.Fatalf("bad tool %q", r.Tool)
			}
			for _, f := range r.Findings {
				if f.abs == "" {
					if c, ok := CleanPath(f.File); !ok || c != f.File {
						t.Fatalf("unclean path %q", f.File)
					}
				} else if !strings.HasPrefix(f.abs, "/") {
					t.Fatalf("relative abs %q", f.abs)
				}
				if f.RuleID == "" || len([]rune(f.Message)) > MaxMessage || f.StartLine < 0 || f.EndLine < f.StartLine {
					t.Fatalf("bad finding %+v", f)
				}
				if f.HelpURI != "" && !strings.HasPrefix(f.HelpURI, "https://") {
					t.Fatalf("bad help link %q", f.HelpURI)
				}
			}
		}
	})
}
