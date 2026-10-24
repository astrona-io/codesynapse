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

const pyApp = `from flask import Flask
from unittest import mock

app = Flask(__name__)

@app.route("/items", methods=["GET", "POST"])
@login_required
def items():
    return run()

@app.get("/health")
def health():
    return "ok"

@router.websocket("/ws")
async def stream(ws):
    pass

@cli.command()
def sync_all():
    pass

@mcp.tool(name="lookup")
def find(q):
    return q

@mock.patch("os.system")
def helper(m):
    pass

@staticmethod
def plain():
    pass
`

const jsApp = `import express from 'express'
const app = express()
// app.get('/commented', handler)
app.get('/users', requireAuth, listUsers)
app.post('/users', async (req, res) => { res.send('ok') })
router.delete("/users/:id",
  requireAdmin,
  removeUser)
axios.get('/api/data', { params: { q } })
api.get('/x', { headers })
server.tool("search", { q: z.string() }, async ({ q }) => ({}))
function listUsers(req, res) {}
`

func TestPythonRoutes(t *testing.T) {
	fr := parsePython([]byte(pyApp))
	got := map[string]route{}
	for _, r := range fr.routes {
		got[r.kind+" "+r.name] = r
	}
	assert.Equal(t, len(got), 6)
	assert.Equal(t, got["http GET /items"].handler, "items")
	assert.Equal(t, got["http POST /items"].guards, []string{"login_required"})
	assert.Equal(t, got["http GET /health"].handler, "health")
	assert.Equal(t, got["http WS /ws"].handler, "stream")
	assert.Equal(t, got["cli sync-all"].handler, "sync_all")
	assert.Equal(t, got["mcp lookup"].handler, "find")
	_, patched := got["http PATCH os.system"]
	assert.False(t, patched, "mock.patch is no route")
}

func TestJSRoutes(t *testing.T) {
	fr := parseJS([]byte(jsApp), false)
	got := map[string]route{}
	for _, r := range fr.routes {
		got[r.kind+" "+r.name] = r
	}
	assert.Equal(t, len(got), 4, "comments, axios and an options object are no routes")
	assert.Equal(t, got["http GET /users"].handler, "listUsers")
	assert.Equal(t, got["http GET /users"].guards, []string{"requireAuth"})
	assert.Equal(t, got["http POST /users"].handler, "", "inline handler")
	assert.Equal(t, got["http DELETE /users/:id"].guards, []string{"requireAdmin"})
	assert.Equal(t, got["http DELETE /users/:id"].handler, "", "removeUser is not a function of this file")
	assert.Equal(t, got["mcp search"].line, 11)
}

// TestRoutesInTheGraph: routes become Route nodes with HANDLED_BY to their
// handler; routes in test files do not count.
func TestRoutesInTheGraph(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.py"), []byte(pyApp), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "server.js"), []byte(jsApp), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test_app.py"), []byte("@app.get(\"/only-in-tests\")\ndef test_x():\n    pass\n"), 0o600))
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	routes, handled := 0, map[string]string{}
	for _, e := range res.Graph.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e.To
		}
	}
	for _, n := range res.Graph.Nodes() {
		if n.Kind != model.KindRoute {
			continue
		}
		routes++
		assert.NotEqual(t, n.Name, "GET /only-in-tests")
		if n.Name == "GET /health" {
			assert.Equal(t, handled[n.ID], model.NodeID("r", model.KindFunction, "app.py::health"))
			assert.Equal(t, n.Signature, model.RouteHTTP)
			assert.Equal(t, n.File, "app.py")
		}
		if n.Name == "GET /users" {
			assert.Equal(t, handled[n.ID], model.NodeID("r", model.KindFunction, "server.js::listUsers"))
			assert.Equal(t, n.Summary, "requireAuth")
		}
	}
	assert.Equal(t, routes, 10)
}
