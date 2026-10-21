package golang

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

func extractSample(t *testing.T) *model.Graph {
	t.Helper()
	root, err := filepath.Abs("testdata/sample")
	require.NoError(t, err)
	res, err := Extract(context.Background(), Options{Repo: "sample", Root: root})
	require.NoError(t, err)
	return res.Graph
}

func hasEdge(g *model.Graph, from, to string, kind model.EdgeKind) bool {
	for _, e := range g.Edges() {
		if e.From == from && e.To == to && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestExtract(t *testing.T) {
	g := extractSample(t)
	id := func(k model.NodeKind, local string) string { return model.NodeID("sample", k, local) }

	run := id(model.KindFunction, "example.com/sample/app.Run")
	helper := id(model.KindFunction, "example.com/sample/app.helper")
	ifaceSave := id(model.KindInterfaceMethod, "(example.com/sample/store.Saver).Save")
	diskSave := id(model.KindMethod, "(*example.com/sample/store.Disk).Save")
	testRun := id(model.KindFunction, "example.com/sample/app.TestRun")

	tests := []struct {
		name     string
		from, to string
		kind     model.EdgeKind
	}{
		{"static call", run, helper, model.EdgeCalls},
		{"interface call", helper, ifaceSave, model.EdgeCalls},
		{"dispatch to impl", ifaceSave, diskSave, model.EdgeDispatchesTo},
		{"implements", id(model.KindType, "example.com/sample/store.Disk"), id(model.KindType, "example.com/sample/store.Saver"), model.EdgeImplements},
		{"test calls func", testRun, run, model.EdgeCalls},
		{"package import", id(model.KindPackage, "example.com/sample/app"), id(model.KindPackage, "example.com/sample/store"), model.EdgeImports},
		{"sensitive external", diskSave, id(model.KindExternal, "os/exec.Command"), model.EdgeCalls},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, hasEdge(g, tt.from, tt.to, tt.kind), "missing %s -[%s]-> %s", tt.from, tt.kind, tt.to)
		})
	}

	n, ok := g.Node(diskSave)
	require.True(t, ok)
	assert.Equal(t, n.File, "store/store.go")
	assert.Equal(t, n.Complexity, 3)
	assert.Contains(t, n.SecurityTags, TagExec)
	assert.NotContains(t, n.Summary, "hunter2secret")
	assert.NotEqual(t, n.BodyHash, "")

	tr, ok := g.Node(testRun)
	require.True(t, ok)
	assert.True(t, tr.IsTest)

	for _, e := range g.Edges() {
		_, okFrom := g.Node(e.From)
		_, okTo := g.Node(e.To)
		assert.True(t, okFrom && okTo, "dangling edge %+v", e)
	}
}

func TestTagFor(t *testing.T) {
	tests := []struct{ pkg, fn, want string }{
		{"os/exec", "os/exec.Command", TagExec},
		{"crypto/tls", "crypto/tls.Dial", TagTLS},
		{"crypto/sha256", "crypto/sha256.Sum256", TagCrypto},
		{"crypto/md5", "crypto/md5.Sum", TagWeakCrypto},
		{"os", "os.Getenv", TagEnv},
		{"strings", "strings.Cut", ""},
		{"database/sql", "(*database/sql.DB).QueryContext", TagSQL},
	}
	for _, tt := range tests {
		t.Run(tt.fn, func(t *testing.T) {
			assert.Equal(t, tagFor(tt.pkg, tt.fn), tt.want)
		})
	}
}

func TestFindings(t *testing.T) {
	g := extractSample(t)
	rules := map[string]model.Node{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding {
			rules[n.Rule] = n
		}
	}
	require.Contains(t, keys(rules), "hardcoded-secret")
	require.Contains(t, keys(rules), "exec-shell")
	assert.Equal(t, rules["exec-shell"].Severity, "critical")
	assert.Equal(t, rules["hardcoded-secret"].CWE, []string{"CWE-798"})
	assert.NotContains(t, rules["hardcoded-secret"].Summary, "s3cr3t")
	// db.Query on a local interface is not database/sql: no sql-concat.
	assert.NotContains(t, keys(rules), "sql-concat")

	owner := model.NodeID("sample", model.KindFunction, "example.com/sample/store.Shell")
	assert.True(t, hasEdge(g, owner, rules["exec-shell"].ID, model.EdgeHasFinding))
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
