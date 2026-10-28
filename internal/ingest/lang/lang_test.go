package lang

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func extract(t *testing.T) (*Result, func(kind model.NodeKind, local string) (model.Node, bool)) {
	t.Helper()
	res, err := Extract(context.Background(), "r", "testdata/repo")
	require.NoError(t, err)
	return res, func(kind model.NodeKind, local string) (model.Node, bool) {
		n, ok := res.Graph.Node(model.NodeID("r", kind, local))
		if !ok {
			return model.Node{}, false
		}
		return *n, true
	}
}

func hasEdge(g *model.Graph, from, to string, kind model.EdgeKind) bool {
	for _, e := range g.Edges() {
		if e.From == from && e.To == to && e.Kind == kind {
			return true
		}
	}
	return false
}

func TestPython(t *testing.T) {
	res, node := extract(t)
	id := func(k model.NodeKind, l string) string { return model.NodeID("r", k, l) }

	svc, ok := node(model.KindType, "svc/app/api.py::UserService")
	require.True(t, ok)
	assert.Equal(t, svc.Summary, "Serves users.")
	assert.Equal(t, svc.Language, Python)

	get, ok := node(model.KindMethod, "svc/app/api.py::UserService.get")
	require.True(t, ok)
	assert.Equal(t, get.StartLine, 15)
	assert.Equal(t, get.EndLine, 19)
	assert.Equal(t, get.Complexity, 2)
	assert.Equal(t, get.QualifiedName, "svc.app.api.UserService.get")

	_, ok = node(model.KindFunction, "svc/app/api.py::not_a_function")
	assert.False(t, ok, "def inside a string is not a declaration")
	_, ok = node(model.KindFunction, "svc/app/api.py::inner")
	assert.False(t, ok, "nested functions belong to their parent")

	g := res.Graph
	assert.True(t, hasEdge(g, get.ID, id(model.KindMethod, "svc/app/api.py::UserService._load"), model.EdgeCalls), "self.method()")
	assert.True(t, hasEdge(g, id(model.KindMethod, "svc/app/api.py::UserService._load"), id(model.KindFunction, "svc/app/db.py::find_user"), model.EdgeCalls), "relative import")
	assert.True(t, hasEdge(g, id(model.KindFile, "svc/app/api.py"), id(model.KindFunction, "svc/app/api.py::run_job"), model.EdgeReferences), "decorated route is an entry point")

	test, ok := node(model.KindFunction, "svc/tests/test_api.py::test_get")
	require.True(t, ok)
	assert.True(t, test.IsTest)
	assert.True(t, hasEdge(g, test.ID, svc.ID, model.EdgeCalls), "absolute import from tests/")

	run, _ := node(model.KindFunction, "svc/app/api.py::run_job")
	assert.Contains(t, run.SecurityTags, "exec")
	assert.Contains(t, keys(res.ExternalImports), "flask")
	assert.Equal(t, res.Languages[Python], 4)
}

func TestTypeScript(t *testing.T) {
	res, node := extract(t)
	id := func(k model.NodeKind, l string) string { return model.NodeID("r", k, l) }
	g := res.Graph

	fu, ok := node(model.KindFunction, "web/src/api/client.ts::fetchUser")
	require.True(t, ok)
	assert.True(t, fu.Exported)
	assert.Equal(t, fu.Summary, "Fetches a user from the API.")
	assert.Equal(t, fu.StartLine, 5)
	assert.Equal(t, fu.EndLine, 8)
	assert.True(t, hasEdge(g, fu.ID, id(model.KindFunction, "web/src/util.ts::formatName"), model.EdgeCalls), "named import")
	assert.Contains(t, fu.SecurityTags, "http")

	list, ok := node(model.KindMethod, "web/src/api/client.ts::Client.list")
	require.True(t, ok)
	assert.Equal(t, list.EndLine, 18)
	parse, ok := node(model.KindMethod, "web/src/api/client.ts::Client.parse")
	require.True(t, ok, "braces in strings, templates and regexes do not break block detection")
	assert.Equal(t, parse.EndLine, 24)
	assert.True(t, hasEdge(g, list.ID, parse.ID, model.EdgeCalls), "this.method()")

	_, ok = node(model.KindFunction, "web/src/api/client.ts::render")
	assert.True(t, ok, "exported arrow function")

	tests, ok := node(model.KindFunction, "web/src/client.test.ts::<tests>")
	require.True(t, ok)
	assert.True(t, tests.IsTest)
	assert.True(t, hasEdge(g, tests.ID, fu.ID, model.EdgeCalls), "describe/it blocks call into code")

	click, ok := node(model.KindFunction, "web/src/components/UserCard.vue::onClick")
	require.True(t, ok)
	assert.Equal(t, click.StartLine, 10, "Vue script lines are offset to the SFC")
	assert.Contains(t, keys(res.ExternalImports), "axios")
	assert.Contains(t, keys(res.ExternalImports), "vitest")
}

func TestFindings(t *testing.T) {
	res, _ := extract(t)
	byRule := map[string][]model.Node{}
	for _, n := range res.Graph.Nodes() {
		if n.Kind == model.KindFinding {
			byRule[n.Rule] = append(byRule[n.Rule], n)
		}
	}
	// The fixture's shell command is built from request.args, so taint
	// reports exec-from-request in place of exec-shell.
	for _, r := range []string{"exec-from-request", "sql-concat", "hardcoded-secret", "code-injection", "dom-xss", "vue-v-html"} {
		assert.Contains(t, keys(byRule), r)
	}
	for _, f := range byRule["hardcoded-secret"] {
		assert.NotContains(t, f.Summary, "s3cr3t")
		assert.NotContains(t, f.Summary, "sk_live")
	}
	assert.NotContains(t, keys(byRule), "exec-shell")
	assert.Equal(t, byRule["exec-from-request"][0].Severity, "critical")
	for _, f := range res.Graph.Nodes() {
		if f.Kind == model.KindFinding {
			assert.NotEqual(t, f.File, "web/src/client.test.ts", "no findings in tests")
		}
	}
}

func TestMaskJS(t *testing.T) {
	src := "const a = 'x{y}' // c { \nconst b = `t ${f({})} {`\nconst r = /\\{/g\n"
	m := maskJS(src)
	assert.Equal(t, len(m), len(src))
	assert.NotContains(t, m, "x{y}")
	assert.Contains(t, m, "f({})", "interpolations stay code")
	assert.Equal(t, depths(m)[2], 0, "braces inside literals do not change depth")
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
