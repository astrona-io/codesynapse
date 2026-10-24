package lang

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestAuditLanguageCoverage(t *testing.T) {
	for _, tt := range []struct{ file, want string }{
		{"api.py", Python}, {"api.ts", TypeScript}, {"api.tsx", TypeScript},
		{"api.js", JavaScript}, {"Api.vue", Vue}, {"Api.java", Java}, {"Api.kt", Kotlin}, {"build.gradle.kts", Kotlin}, {"api.rs", Rust}, {"Api.cs", ""}, {"api.d.ts", ""},
	} {
		t.Run(tt.file, func(t *testing.T) { assert.Equal(t, Language(tt.file), tt.want) })
	}
}

func TestAuditPartialStructuralCoverage(t *testing.T) {
	for _, tt := range []struct {
		name, language, source string
		want                   []string
	}{
		{"python partial parameters", Python, "def retained():\n    return 1\ndef partial(\n", []string{"retained", "partial"}},
		{"python unterminated string", Python, "def retained():\n    return 1\n\"\"\"\ndef hidden():\n    pass\n", []string{"retained"}},
		{"typescript partial body", TypeScript, "export function retained() { return 1 }\nexport function partial() {\n", []string{"retained", "partial"}},
		{"typescript partial parameters", TypeScript, "export function retained() { return 1 }\nexport function partial(\n", []string{"retained", "partial"}},
		{"typescript unclosed earlier block", TypeScript, "export function first() {\nexport function swallowed() { return 1 }\n", []string{"first"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var result *fileResult
			if tt.language == Python {
				result = parsePython([]byte(tt.source))
			} else {
				result = parseJS([]byte(tt.source), true)
			}
			var names []string
			for _, symbol := range result.symbols {
				names = append(names, symbol.name)
				assert.True(t, symbol.start >= 1 && symbol.end >= symbol.start && symbol.end <= strings.Count(tt.source, "\n")+1)
			}
			assert.Equal(t, names, tt.want)
		})
	}
}

func TestAuditUnknownReceiverNameFallback(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "calls.py"), []byte(`class Local:
    def save(self):
        pass

def caller(foreign):
    foreign.save()
`), 0o600))
	res, err := Extract(context.Background(), "audit", root)
	require.NoError(t, err)
	from := model.NodeID("audit", model.KindFunction, "calls.py::caller")
	to := model.NodeID("audit", model.KindMethod, "calls.py::Local.save")
	assert.True(t, hasEdge(res.Graph, from, to, model.EdgeCalls), "audit baseline: unique-name fallback does not prove the receiver's type")
	for _, e := range res.Graph.Edges() {
		if e.From == from && e.To == to {
			assert.Equal(t, e.Evidence, model.EvidenceInferred, "4j.4: the guess is labelled, not presented as resolved")
		}
	}
}

func BenchmarkAuditStructuralParse(b *testing.B) {
	for _, language := range []string{Python, TypeScript} {
		for _, functions := range []int{10, 1000} {
			b.Run(fmt.Sprintf("%s/functions_%d", language, functions), func(b *testing.B) {
				var source strings.Builder
				for i := 0; i < functions; i++ {
					if language == Python {
						fmt.Fprintf(&source, "def f%d(value):\n    if value < 0:\n        return -value\n    return value + %d\n\n", i, i)
					} else {
						fmt.Fprintf(&source, "export function f%d(value: number) {\n  if (value < 0) { return -value }\n  return value + %d\n}\n", i, i)
					}
				}
				input := []byte(source.String())
				b.ReportAllocs()
				b.SetBytes(int64(len(input)))
				for b.Loop() {
					var result *fileResult
					if language == Python {
						result = parsePython(input)
					} else {
						result = parseJS(input, true)
					}
					require.Equal(b, len(result.symbols), functions)
				}
			})
		}
	}
}
