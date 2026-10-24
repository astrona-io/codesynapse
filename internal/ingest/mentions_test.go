package ingest

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// Documentation is linked after every source language is in the graph, so a
// README that names a Python, JavaScript, TypeScript or Vue symbol gets the
// same MENTIONS edge as one that names a Go symbol.
func TestDocsMentionSymbolsOfEverySourceLanguage(t *testing.T) {
	root := policyFiles(t, map[string]string{
		"go.mod":      "module example.com/app\n\ngo 1.22\n",
		"main.go":     "package main\n\n// Serve starts it.\nfunc Serve() {}\n\nfunc main() { Serve() }\n",
		"app.js":      "export function greetJS(name) { return 'hi ' + name; }\n",
		"util.ts":     "export function slugifyTS(text: string): string { return text.toLowerCase(); }\n",
		"tool.py":     "def render_py(items):\n    return items\n",
		"README.md":   "# App\n\nCall `Serve`, `greetJS`, `slugifyTS` and `render_py` to get going.\n",
		"docs/two.md": "# Two\n\nMentions `Nope` only.\n",
	})
	g, err := Run(context.Background(), Options{Repo: "r", Root: root})
	require.NoError(t, err)

	byID := map[string]model.Node{}
	for _, n := range g.Nodes() {
		byID[n.ID] = n
	}
	mentioned := map[string]bool{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeMentions {
			if n, ok := byID[e.To]; ok {
				mentioned[n.Name] = true
			}
		}
	}
	for _, name := range []string{"Serve", "greetJS", "slugifyTS", "render_py"} {
		assert.True(t, mentioned[name], name+" is mentioned by the README")
	}
	assert.False(t, mentioned["Nope"])
}
