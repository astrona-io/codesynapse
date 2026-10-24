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

// Routes registered in one file and handled in another: the handler is
// resolved through the registering file's imports, and a controller class
// named only by a string is found when the repository declares it once.
var crossFileRoutes = map[string]string{
	"web/routes.js": `import { listUsers } from './users'
import * as posts from './posts'
const router = express.Router()
router.get('/u', listUsers)
router.get('/p', requireAuth, posts.list)
`,
	"web/users.js": "export function listUsers(req, res) { res.json([]) }\n",
	"web/posts.js": "export function list(req, res) { res.json([]) }\n",

	"site/blog/__init__.py": "",
	"site/blog/urls.py": `from django.urls import include, path
from . import views
from .views import AboutView

urlpatterns = [
    path("posts/<int:pk>/", views.detail, name="detail"),
    path("about/", AboutView.as_view()),
    path("api/", include("api.urls")),
]
`,
	"site/blog/views.py": `def detail(request, pk):
    return None


class AboutView(TemplateView):
    template_name = "about.html"
`,

	"Api/Program.cs": `namespace Shop.Api;

app.MapGet("/orders", OrderHandlers.List);
`,
	"Api/OrderHandlers.cs": `namespace Shop.Api;

public static class OrderHandlers
{
    public static IResult List() => Results.Ok();
}
`,

	"routes/api.php": `<?php
Route::get('/legacy', 'LegacyController@index');
`,
	"app/Http/Controllers/LegacyController.php": `<?php
namespace App\Http\Controllers;

class LegacyController
{
    public function index() {}
}
`,
}

func TestRouteHandlersAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range crossFileRoutes {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	g := res.Graph
	handled := map[string]model.Edge{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e
		}
	}
	got := map[string]string{}
	evidence := map[string]model.Evidence{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRoute {
			got[n.File+" "+n.Name] = handled[n.ID].To
			evidence[n.File+" "+n.Name] = handled[n.ID].Evidence
		}
	}
	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	assert.Equal(t, got, map[string]string{
		"web/routes.js GET /u":               id(model.KindFunction, "web/users.js", "listUsers"),
		"web/routes.js GET /p":               id(model.KindFunction, "web/posts.js", "list"),
		"site/blog/urls.py /posts/<int:pk>/": id(model.KindFunction, "site/blog/views.py", "detail"),
		"site/blog/urls.py /about/":          id(model.KindType, "site/blog/views.py", "AboutView"),
		"Api/Program.cs GET /orders":         id(model.KindMethod, "Api/OrderHandlers.cs", "OrderHandlers.List"),
		"routes/api.php GET /legacy":         id(model.KindMethod, "app/Http/Controllers/LegacyController.php", "LegacyController.index"),
	})
	assert.Equal(t, evidence["web/routes.js GET /u"], model.EvidenceSyntactic, "through the import")
	assert.Equal(t, evidence["routes/api.php GET /legacy"], model.EvidenceInferred, "by the class name only")
}
