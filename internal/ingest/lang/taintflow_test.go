package lang

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// repoTaint extracts a repository of files and returns its request-input
// findings as "file:line rule", sorted, with their summaries.
func repoTaint(t *testing.T, files map[string]string) ([]string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}
	res, err := Extract(context.Background(), "r", dir)
	require.NoError(t, err)
	var out []string
	summary := map[string]string{}
	for _, n := range res.Graph.Nodes() {
		if n.Kind != model.KindFinding || !slices.Contains(taintRules, n.Rule) {
			continue
		}
		k := fmt.Sprintf("%s:%d %s", n.File, n.StartLine, n.Rule)
		out = append(out, k)
		summary[k] = n.Summary
	}
	slices.Sort(out)
	return out, summary
}

const (
	pyHelper = "import os\n\ndef find_user(name):\n    cursor.execute(\"SELECT * FROM users WHERE name = '%s'\" % name)\n\n" +
		"def clean(n):\n    return 42\n\ndef echo(n):\n    return n\n"
	pyView       = "from flask import request\nfrom db import find_user, clean, echo\n\n@app.route('/u')\ndef show():\n"
	jsExecHelper = "const { exec } = require('child_process')\n\nfunction runCmd(cmd) {\n  exec('ls ' + cmd)\n}\n\n" +
		"function constant(cmd) {\n  exec('ls')\n}\n\nmodule.exports = { runCmd, constant }\n"
	jsExpress = "const express = require('express')\nconst { runCmd, constant } = require('./helpers')\nconst app = express()\n\n" +
		"app.get('/run', (req, res) => {\n"
)

func TestTaintAcrossFunctions(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  []string
		// in is what the summary of the first finding must say.
		in []string
	}{
		{
			name:  "python helper builds SQL from request input",
			files: map[string]string{"db.py": pyHelper, "app.py": pyView + "    find_user(request.args['name'])\n"},
			want:  []string{"db.py:4 sql-from-request"},
			in:    []string{"via request.args in show", "through find_user"},
		},
		{
			name:  "python keyword argument",
			files: map[string]string{"db.py": pyHelper, "app.py": pyView + "    n = request.args['name']\n    find_user(name=n)\n"},
			want:  []string{"db.py:4 sql-from-request"},
		},
		{
			name:  "python sanitised before the call",
			files: map[string]string{"db.py": pyHelper, "app.py": pyView + "    find_user(int(request.args['id']))\n"},
		},
		{
			name:  "python constant argument",
			files: map[string]string{"db.py": pyHelper, "app.py": pyView + "    n = request.args['name']\n    find_user('bob')\n"},
		},
		{
			name:  "python call not imported",
			files: map[string]string{"db.py": pyHelper, "app.py": "from flask import request\n\n@app.route('/u')\ndef show():\n    lookup(request.args['name'])\n"},
		},
		{
			name:  "python result of a function that drops its argument is clean",
			files: map[string]string{"db.py": pyHelper, "app.py": pyView + "    cursor.execute('SELECT ' + clean(request.args['n']))\n"},
		},
		{
			name:  "python result of a function that returns its argument is tainted",
			files: map[string]string{"db.py": pyHelper, "app.py": pyView + "    cursor.execute('SELECT ' + echo(request.args['n']))\n"},
			want:  []string{"app.py:6 sql-from-request"},
		},
		{
			name: "python function returning request input",
			files: map[string]string{
				"util.py": "from flask import request\n\ndef wanted():\n    return request.args['n']\n",
				"app.py":  "from util import wanted\n\n@app.route('/u')\ndef show():\n    os.system('echo ' + wanted())\n",
			},
			want: []string{"app.py:5 exec-from-request"},
			in:   []string{"returned by wanted"},
		},
		{
			name: "python method through self",
			files: map[string]string{"app.py": "from flask import request\n\nclass Store:\n    def get(self):\n        self.load(request.args['p'])\n\n" +
				"    def load(self, p):\n        return open(p)\n"},
			want: []string{"app.py:8 path-from-request"},
			in:   []string{"in Store.get", "through Store.load"},
		},
		{
			name:  "express handler passes a query value to a helper running exec",
			files: map[string]string{"helpers.js": jsExecHelper, "app.js": jsExpress + "  runCmd(req.query.x)\n})\n"},
			want:  []string{"helpers.js:4 exec-from-request"},
			in:    []string{"via req.query in a callback in app.js", "through runCmd"},
		},
		{
			name:  "express value parsed as a number",
			files: map[string]string{"helpers.js": jsExecHelper, "app.js": jsExpress + "  runCmd(parseInt(req.query.x))\n})\n"},
		},
		{
			name:  "express helper that ignores its argument",
			files: map[string]string{"helpers.js": jsExecHelper, "app.js": jsExpress + "  constant(req.query.x)\n})\n"},
		},
		{
			name: "js call resolved by name only is not followed",
			files: map[string]string{"helpers.js": jsExecHelper, "app.js": "const express = require('express')\nconst app = express()\n\n" +
				"app.get('/run', (req, res) => {\n  runCmd(req.query.x)\n})\n"},
		},
		{
			name: "typescript import through a variable and a second helper",
			files: map[string]string{
				"src/shell.ts": "import { execSync } from 'child_process'\n\nexport function run(cmd: string, opts?: object) {\n  return execSync(`sh -c ${cmd}`)\n}\n",
				"src/svc.ts":   "import { run } from './shell'\n\nexport const build = (target: string) => {\n  run('make ' + target)\n}\n",
				"src/app.ts":   "import { build } from './svc'\n\nexport async function handler(req, res) {\n  const t = req.body.target\n  build(t)\n}\n",
			},
			want: []string{"src/shell.ts:4 exec-from-request"},
			in:   []string{"in handler", "through build → run"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, summary := repoTaint(t, tt.files)
			assert.Equal(t, strings.Join(got, ", "), strings.Join(tt.want, ", "))
			if len(tt.in) > 0 && len(got) > 0 {
				for _, s := range tt.in {
					assert.True(t, strings.Contains(summary[got[0]], s), summary[got[0]]+" does not say "+s)
				}
			}
		})
	}
}

func TestTaintAcrossHops(t *testing.T) {
	chain := func(n int) map[string]string {
		var b strings.Builder
		b.WriteString("from flask import request\n\n@app.route('/x')\ndef view():\n    h1(request.args['p'])\n\n")
		for i := 1; i < n; i++ {
			fmt.Fprintf(&b, "def h%d(p):\n    h%d(p)\n\n", i, i+1)
		}
		fmt.Fprintf(&b, "def h%d(p):\n    os.system(p)\n", n)
		return map[string]string{"app.py": b.String()}
	}
	got, summary := repoTaint(t, chain(3))
	require.Equal(t, len(got), 1)
	assert.True(t, strings.Contains(summary[got[0]], "through h1 → h2 → h3"), summary[got[0]])
	got, _ = repoTaint(t, chain(4))
	assert.Equal(t, len(got), 0, "a path of four calls is past the limit")
}

func TestTaintDjangoURLParameters(t *testing.T) {
	got, summary := repoTaint(t, map[string]string{
		"shop/__init__.py": "",
		"shop/urls.py": "from django.urls import path, re_path\nfrom . import views\n\nurlpatterns = [\n" +
			"    path('users/<str:name>/', views.detail),\n" +
			"    re_path(r'^items/(?P<slug>[\\w-]+)/(?P<page>\\d+)/$', views.item),\n" +
			"    path('n/<int:id>/', views.num),\n" +
			"    path('p/<path:rest>/', views.Page.as_view()),\n]\n",
		"shop/views.py": "import os\n\ndef detail(request, name):\n    cursor.execute(\"SELECT * FROM t WHERE n = '%s'\" % name)\n\n" +
			"def item(request, slug, page):\n    os.system('echo ' + slug)\n    os.system('echo ' + page)\n\n" +
			"def num(request, id):\n    cursor.execute('SELECT * FROM t WHERE id = %s' % id)\n",
	})
	assert.Equal(t, strings.Join(got, ", "), "shop/views.py:4 sql-from-request, shop/views.py:7 exec-from-request")
	assert.True(t, strings.Contains(summary["shop/views.py:4 sql-from-request"], "route parameter name"), summary["shop/views.py:4 sql-from-request"])
}

func TestDjangoParams(t *testing.T) {
	for _, tt := range []struct {
		pattern string
		want    []string
	}{
		{"/users/<name>/", []string{"name"}},
		{"/users/<str:name>/<int:id>/<uuid:u>/<slug:s>", []string{"name", "s"}},
		{`/^items/(?P<slug>[\w-]+)/(?P<page>\d+)/(?P<year>[0-9]{4})$`, []string{"slug"}},
		{`/^a/(?P<x>(?:a|b)+)/$`, []string{"x"}},
		{"/static/", nil},
		{"/(?P<", nil},
	} {
		t.Run(tt.pattern, func(t *testing.T) {
			assert.Equal(t, strings.Join(djangoParams(tt.pattern), ","), strings.Join(tt.want, ","))
		})
	}
}

func TestTaintParams(t *testing.T) {
	py := parsePython([]byte("class A:\n    def m(self, a, /, b: int = 1, *args, c=None, **kw):\n        pass\n\ndef f(\n    x,\n    y,\n):\n    pass\n"))
	params := map[string]string{}
	for _, s := range py.symbols {
		params[s.name] = strings.Join(s.params, ",")
	}
	assert.Equal(t, params["A.m"], "self,a,b,*,c,*")
	assert.Equal(t, params["f"], "x,y")
	js := parseJS([]byte("export function f(a, { b }, [c], d = 1, ...rest) {}\nconst g = x => x\n"+
		"const h = async (p: string, q?: number) => {}\nclass C {\n  constructor(private readonly s: S, t) {}\n  m(this: C, u) {}\n}\n"), true)
	for _, s := range js.symbols {
		params[s.name] = strings.Join(s.params, ",")
	}
	assert.Equal(t, params["f"], "a,,,d,*")
	assert.Equal(t, params["g"], "x")
	assert.Equal(t, params["h"], "p,q")
	assert.Equal(t, params["C.constructor"], "s,t")
	assert.Equal(t, params["C.m"], "u")
}

func FuzzDjangoParams(f *testing.F) {
	for _, s := range []string{"/users/<str:name>/", `/^(?P<slug>[\w-]+)/(?P<n>\d+)$`, "/(?P<", "/(?P<a>((("} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, pattern string) {
		for _, p := range djangoParams(pattern) {
			if p == "" || !strings.Contains(pattern, p) {
				t.Fatalf("%q from %q", p, pattern)
			}
		}
	})
}

// FuzzTaintAcross links fuzzed callers, and a fuzzed urls.py, against
// helpers with sinks.
func FuzzTaintAcross(f *testing.F) {
	f.Add(pyView + "    find_user(name=request.args['n'], *a)\n")
	f.Add(jsExpress + "  runCmd(req.query.x, ...rest)\n})\n")
	f.Add("urlpatterns = [path('<p>/', detail), re_path(r'^(?P<q>(a)/$', echo)]\nfrom db import echo\ndef detail(request, p):\n    return echo(echo(echo(echo(echo(p)))))\n")
	f.Fuzz(func(_ *testing.T, src string) {
		ix := &indexer{repo: "r", g: model.NewGraph(), files: map[string]*parsedFile{}, res: &Result{ExternalImports: map[string][]string{}, ExternalCalls: map[string][]string{}, Languages: map[string]int{}}}
		ix.parse("db.py", Python, []byte(pyHelper))
		ix.parse("helpers.js", JavaScript, []byte(jsExecHelper))
		ix.parse("app.py", Python, []byte(src))
		ix.parse("urls.py", Python, []byte(src))
		ix.parse("app.ts", TypeScript, []byte(src))
		ix.link()
	})
}
