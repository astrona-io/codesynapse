package lang

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// evidenceFiles is the labelled structural fixture. foreign.save() has no
// knowable receiver; w.render() really is Widget.render.
var evidenceFiles = map[string]string{
	"b.py": "def helper():\n    pass\n",
	"a.py": `from b import helper

class Local:
    def save(self):
        self.flush()

    def flush(self):
        pass

def local_fn():
    pass

def caller(foreign):
    foreign.save()
    helper()
    local_fn()
`,
	"widget.ts": `export class Widget {
  render() { return this.draw() }
  draw() { return 1 }
}
`,
	"page.ts": `import { Widget } from './widget'
export function page(w: Widget) {
  return w.render()
}
export function boot() { return page(new Widget()) }
`,
	"broken.ts":  "export function open() {\n  if (x) {\n",
	"unended.py": "def first():\n    pass\n\"\"\"\ndef hidden():\n",
}

type callEdge struct{ from, to string }

// evidenceTruth lists by hand every call the fixture makes between its own
// symbols, as file::symbol pairs.
var evidenceTruth = map[callEdge]bool{
	{"a.py::Local.save", "a.py::Local.flush"}:              true,
	{"a.py::caller", "b.py::helper"}:                       true,
	{"a.py::caller", "a.py::local_fn"}:                     true,
	{"widget.ts::Widget.render", "widget.ts::Widget.draw"}: true,
	{"page.ts::page", "widget.ts::Widget.render"}:          true,
	{"page.ts::boot", "page.ts::page"}:                     true,
	{"page.ts::boot", "widget.ts::Widget"}:                 true, // new Widget()
}

func extractEvidence(t *testing.T) *model.Graph {
	t.Helper()
	root := t.TempDir()
	for name, src := range evidenceFiles {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(src), 0o600))
	}
	res, err := Extract(context.Background(), "ev", root)
	require.NoError(t, err)
	return res.Graph
}

// TestEvidencePrecisionRecall measures each evidence level against the
// labels. The numbers are reported in docs/EDGE_EVIDENCE.md; a change that
// moves them must update both.
func TestEvidencePrecisionRecall(t *testing.T) {
	g := extractEvidence(t)
	local := func(id string) string {
		n, _ := g.Node(id)
		return n.File + "::" + n.QualifiedName[len(n.Package)+1:]
	}
	type tally struct{ found, correct int }
	byEvidence := map[model.Evidence]*tally{}
	recalled := 0
	for _, e := range g.Edges() {
		if e.Kind != model.EdgeCalls {
			continue
		}
		require.True(t, e.Evidence == model.EvidenceSyntactic || e.Evidence == model.EvidenceInferred, "structural edges are never typed or unknown")
		tl := byEvidence[e.Evidence]
		if tl == nil {
			tl = &tally{}
			byEvidence[e.Evidence] = tl
		}
		tl.found++
		if evidenceTruth[callEdge{local(e.From), local(e.To)}] {
			tl.correct++
			recalled++
		}
	}
	assert.Equal(t, *byEvidence[model.EvidenceSyntactic], tally{found: 6, correct: 6})
	assert.Equal(t, *byEvidence[model.EvidenceInferred], tally{found: 2, correct: 1})
	assert.Equal(t, recalled, len(evidenceTruth))
}

func TestStructuralAnalysisStatus(t *testing.T) {
	g := extractEvidence(t)
	for file, want := range map[string]string{
		"a.py": model.StatusComplete, "page.ts": model.StatusComplete,
		"broken.ts": model.StatusPartial, "unended.py": model.StatusPartial,
	} {
		t.Run(file, func(t *testing.T) {
			n, ok := g.Node(model.NodeID("ev", model.KindFile, file))
			require.True(t, ok)
			a, err := model.DecodeAnalysis(n.Analysis)
			require.NoError(t, err)
			assert.Equal(t, a.Level, model.LevelSyntactic)
			assert.Equal(t, a.Status, want)
		})
	}
}
