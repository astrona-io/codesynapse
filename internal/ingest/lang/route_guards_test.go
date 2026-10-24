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

// Guards and prefixes that come from outside the registration line: router
// middleware, route groups, controller callbacks, view decorators and
// include() chains; and NestJS controllers and commander commands.
var guardedRoutes = map[string]string{
	"web/routes.js": `import express from 'express'
import { requireAuth } from './auth'
const router = express.Router()
router.get('/public', (req, res) => res.send('ok'))
router.use(requireAuth)
router.use(express.json())
router.get('/private', (req, res) => res.send('secret'))
`,
	"web/cli.js": `const { program } = require('commander')
program
  .command('sync <dir>')
  .description('copy files')
  .action(syncAll)
program.command('serve').action(() => {})
function syncAll(dir) {}
`,
	"web/users.controller.ts": `@Controller('users')
@UseGuards(AuthGuard('jwt'))
export class UsersController {
  @Get(':id')
  findOne(@Param('id') id: string) {
    return id
  }

  @Post()
  @Roles('admin')
  create() {}
}
`,

	"routes/web.php": `<?php
use App\Http\Controllers\PhotoController;
use App\Http\Controllers\AdminController;

Route::get('/', function () { return view('welcome'); });
Route::prefix('admin')->middleware(['auth', 'can:admin'])->group(function () {
    Route::get('/stats', [AdminController::class, 'stats']);
    Route::controller(AdminController::class)->group(function () {
        Route::post('/purge', 'purge');
    });
});
Route::resource('photos', PhotoController::class)->only(['index', 'show']);
`,
	"app/Http/Controllers/AdminController.php": `<?php
namespace App\Http\Controllers;

class AdminController
{
    public function stats() {}
    public function purge() {}
}
`,
	"app/Http/Controllers/PhotoController.php": `<?php
namespace App\Http\Controllers;

class PhotoController
{
    public function index() {}
    public function show($photo) {}
}
`,

	"app/controllers/application_controller.rb": `class ApplicationController < ActionController::Base
  before_action :authenticate_user!
end
`,
	"app/controllers/pages_controller.rb": `class PagesController < ApplicationController
  skip_before_action :authenticate_user!, only: [:home]
  before_action :load_page, except: [:home]

  def home; end

  def show
  end

  private

  def load_page; end
end
`,
	"config/routes.rb": `Rails.application.routes.draw do
  root "pages#home"
  get "pages/:id", to: "pages#show"
end
`,

	"site/urls.py": `from django.urls import include, path

urlpatterns = [path("blog/", include("blog.urls"))]
`,
	"site/blog/__init__.py": "",
	"site/blog/urls.py": `from django.urls import path
from . import views

urlpatterns = [
    path("posts/<int:pk>/", views.detail),
    path("drafts/", views.Drafts.as_view()),
]
`,
	"site/blog/views.py": `from django.contrib.auth.decorators import login_required


@login_required
def detail(request, pk):
    return None


class Drafts(LoginRequiredMixin, ListView):
    pass
`,
}

func TestRouteGuardsAndPrefixes(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range guardedRoutes {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	handled := map[string]string{}
	for _, e := range res.Graph.Edges() {
		if e.Kind == model.EdgeHandledBy {
			handled[e.From] = e.To
		}
	}
	type got struct{ handler, guards string }
	routes := map[string]got{}
	for _, n := range res.Graph.Nodes() {
		if n.Kind == model.KindRoute {
			routes[n.File+" "+n.Signature+" "+n.Name] = got{handled[n.ID], n.Summary}
		}
	}
	id := func(k model.NodeKind, rel, name string) string { return model.NodeID("r", k, rel+"::"+name) }
	nest, admin, photo := "web/users.controller.ts", "app/Http/Controllers/AdminController.php", "app/Http/Controllers/PhotoController.php"
	pages := "app/controllers/pages_controller.rb"
	assert.Equal(t, routes, map[string]got{
		"web/routes.js http GET /public":  {"", ""},
		"web/routes.js http GET /private": {"", "requireAuth"},
		"web/cli.js cli sync":             {id(model.KindFunction, "web/cli.js", "syncAll"), ""},
		"web/cli.js cli serve":            {"", ""},
		nest + " http GET /users/:id":     {id(model.KindMethod, nest, "UsersController.findOne"), "UseGuards(AuthGuard('jwt'))"},
		nest + " http POST /users":        {id(model.KindMethod, nest, "UsersController.create"), "UseGuards(AuthGuard('jwt')); Roles('admin')"},

		"routes/web.php http GET /":               {"", ""},
		"routes/web.php http GET /admin/stats":    {id(model.KindMethod, admin, "AdminController.stats"), "middleware(auth); middleware(can admin)"},
		"routes/web.php http POST /admin/purge":   {id(model.KindMethod, admin, "AdminController.purge"), "middleware(auth); middleware(can admin)"},
		"routes/web.php http GET /photos":         {id(model.KindMethod, photo, "PhotoController.index"), ""},
		"routes/web.php http GET /photos/{photo}": {id(model.KindMethod, photo, "PhotoController.show"), ""},

		"config/routes.rb http GET /":          {id(model.KindMethod, pages, "PagesController.home"), ""},
		"config/routes.rb http GET /pages/:id": {id(model.KindMethod, pages, "PagesController.show"), "before_action :authenticate_user!"}, // load_page loads, it does not check access,

		"site/blog/urls.py http /blog/posts/<int:pk>/": {id(model.KindFunction, "site/blog/views.py", "detail"), "login_required"},
		"site/blog/urls.py http /blog/drafts/":         {id(model.KindType, "site/blog/views.py", "Drafts"), "LoginRequiredMixin"},
	})
}
