package lang

import (
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/rules"
)

var taintRules = []string{"sql-from-request", "exec-from-request", "path-from-request", "ssrf-from-request"}

// taintFound returns the request-input rules a file reports, sorted.
func taintFound(lang, src string) []string {
	var fr *fileResult
	if lang == Python {
		fr = parsePython([]byte(src))
	} else {
		fr = parseJS([]byte(src), lang == TypeScript)
	}
	var out []string
	for _, f := range fr.findings {
		if slices.Contains(taintRules, f.rule) {
			out = append(out, f.rule)
		}
	}
	slices.Sort(out)
	return out
}

func TestPythonTaint(t *testing.T) {
	const flask = "from flask import request\nimport subprocess, os, requests, shlex\n\n@app.route('/x')\ndef view():\n"
	for _, tt := range []struct {
		name, body string
		want       []string
	}{
		{"sql via variable and f-string", "    name = request.args.get('name')\n    q = f\"SELECT * FROM users WHERE name = '{name}'\"\n    cursor.execute(q)\n", []string{"sql-from-request"}},
		{"sql via % formatting", "    uid = request.form['id']\n    cursor.execute(\"SELECT * FROM t WHERE id = %s\" % uid)\n", []string{"sql-from-request"}},
		{"sql via .format", "    uid = request.values.get('id')\n    db.execute(\"DELETE FROM t WHERE id = {}\".format(uid))\n", []string{"sql-from-request"}},
		{"sql parameterised", "    name = request.args.get('name')\n    cursor.execute(\"SELECT * FROM users WHERE name = %s\", (name,))\n", nil},
		{"sql int conversion", "    uid = int(request.args['id'])\n    cursor.execute(f\"SELECT * FROM t WHERE id = {uid}\")\n", nil},
		{"sql typed get", "    uid = request.args.get('id', type=int)\n    cursor.execute(f\"SELECT * FROM t WHERE id = {uid}\")\n", nil},
		{"sql constant", "    cursor.execute(\"SELECT * FROM t\")\n", nil},
		{"sql multi-line call", "    q = request.json['q']\n    cursor.execute(\n        \"SELECT \" + q,\n    )\n", []string{"sql-from-request"}},
		{"django raw", "    s = request.GET.get('s')\n    User.objects.raw('SELECT * FROM u WHERE s = ' + s)\n", []string{"sql-from-request"}},
		{"exec shell", "    host = request.args['host']\n    subprocess.run('ping -c1 ' + host, shell=True)\n", []string{"exec-from-request"}},
		{"exec argument list", "    ref = request.args['ref']\n    subprocess.run(['git', 'log', ref])\n", []string{"exec-from-request"}},
		{"exec os.system", "    os.system('rm ' + request.args['f'])\n", []string{"exec-from-request"}},
		{"exec shlex.quote", "    host = request.args['host']\n    subprocess.run('ping -c1 ' + shlex.quote(host), shell=True)\n", nil},
		{"exec constant", "    subprocess.run(['uptime'])\n", nil},
		{"path open", "    name = request.args['file']\n    path = os.path.join('/srv/files', name)\n    return open(path).read()\n", []string{"path-from-request"}},
		{"path basename", "    name = os.path.basename(request.args['file'])\n    return open(os.path.join('/srv/files', name)).read()\n", nil},
		{"path constant", "    return open('/srv/files/index.html').read()\n", nil},
		{"ssrf requests", "    target = request.args['url']\n    return requests.get(target).text\n", []string{"ssrf-from-request"}},
		{"ssrf keyword", "    return requests.request('GET', url=request.json['url']).text\n", []string{"ssrf-from-request"}},
		{"ssrf fixed host", "    uid = request.args['id']\n    return requests.get(f\"https://api.example.com/users/{uid}\").text\n", nil},
		{"ssrf constant", "    return requests.get('https://example.com').text\n", nil},
		{"clean reassignment", "    q = request.args['q']\n    q = 'SELECT 1'\n    cursor.execute(q)\n", nil},
		{"comparison is clean", "    admin = request.args.get('a') == '1'\n    cursor.execute('SELECT %s' % admin)\n", nil},
		{"string inside literal", "    cursor.execute(\"SELECT request.args FROM t\")\n", nil},
		{"loop variable", "    for key in request.args:\n        cursor.execute('SELECT ' + key)\n", []string{"sql-from-request"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, taintFound(Python, flask+tt.body), tt.want)
		})
	}
}

func TestPythonRouteParameters(t *testing.T) {
	for _, tt := range []struct {
		name, src string
		want      []string
	}{
		{"flask path", "@app.route('/files/<path:name>')\ndef get(name):\n    return open('/srv/' + name).read()\n", []string{"path-from-request"}},
		{"flask int converter", "@app.route('/u/<int:uid>')\ndef get(uid):\n    cursor.execute(f'SELECT * FROM t WHERE id = {uid}')\n", nil},
		{"fastapi str query", "@router.get('/search')\nasync def search(q: str, db = Depends(get_db)):\n    db.execute(f\"SELECT * FROM t WHERE n = '{q}'\")\n", []string{"sql-from-request"}},
		{"fastapi int path", "@app.get('/items/{item_id}')\ndef item(item_id: int):\n    cursor.execute(f'SELECT * FROM t WHERE id = {item_id}')\n", nil},
		{"fastapi Query()", "@app.get('/fetch')\ndef fetch(\n    url=Query(None),\n):\n    return httpx.get(url)\n", []string{"ssrf-from-request"}},
		{"not a route", "def helper(name: str):\n    cursor.execute('SELECT ' + name)\n", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, taintFound(Python, tt.src), tt.want)
		})
	}
}

func TestJSTaint(t *testing.T) {
	const head = "const { exec, spawn } = require('child_process')\nconst fs = require('fs')\nconst path = require('path')\n\n"
	wrap := func(body string) string {
		return head + "app.get('/x', async (req, res) => {\n" + body + "})\n"
	}
	for _, tt := range []struct {
		name, body string
		want       []string
	}{
		{"sql template literal", "  const id = req.query.id\n  const rows = await db.query(`SELECT * FROM t WHERE id = ${id}`)\n", []string{"sql-from-request"}},
		{"sql concatenation", "  const { name } = req.body\n  const sql = 'SELECT * FROM u WHERE name = \\'' + name + '\\''\n  await knex.raw(sql)\n", []string{"sql-from-request"}},
		{"sql sequelize", "  await sequelize.query(\"SELECT * FROM t WHERE n = '\" + req.params.n + \"'\")\n", []string{"sql-from-request"}},
		{"sql parameterised", "  const id = req.query.id\n  await db.query('SELECT * FROM t WHERE id = $1', [id])\n", nil},
		{"sql Number", "  const id = Number(req.query.id)\n  await db.query(`SELECT * FROM t WHERE id = ${id}`)\n", nil},
		{"sql parseInt", "  const page = parseInt(req.query.page, 10)\n  await db.query('SELECT * FROM t OFFSET ' + page)\n", nil},
		{"sql constant", "  await db.query('SELECT 1')\n", nil},
		{"exec template", "  const host = req.query.host\n  exec(`ping -c1 ${host}`)\n", []string{"exec-from-request"}},
		{"exec spawn argument", "  spawn('git', ['log', req.body.ref])\n", []string{"exec-from-request"}},
		{"exec constant", "  exec('uptime')\n", nil},
		{"exec regexp is not child_process", "  const m = /a/.exec(req.query.s)\n", nil},
		{"path readFile", "  const file = req.params.file\n  fs.readFile('/srv/' + file, cb)\n", []string{"path-from-request"}},
		{"path join", "  res.sendFile(path.join(__dirname, 'public', req.query.f))\n", []string{"path-from-request"}},
		{"path basename", "  const file = path.basename(req.params.file)\n  fs.readFile(path.join('/srv', file), cb)\n", nil},
		{"path constant", "  fs.readFile('/srv/index.html', cb)\n", nil},
		{"ssrf fetch", "  const target = req.query.url\n  const r = await fetch(target)\n", []string{"ssrf-from-request"}},
		{"ssrf axios", "  await axios.get(req.body.callback)\n", []string{"ssrf-from-request"}},
		{"ssrf fixed host", "  await fetch(`https://api.example.com/users/${req.params.id}`)\n", nil},
		{"ssrf encodeURIComponent", "  await fetch('https://' + 'api.example.com/u/' + encodeURIComponent(req.params.id))\n", nil},
		{"ssrf constant", "  await fetch('https://example.com')\n", nil},
		{"object key is not the value", "  const id = req.query.id\n  await db.query({ id: 1 }.sql)\n", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, taintFound(TypeScript, wrap(tt.body)), tt.want)
		})
	}
}

func TestJSTaintScope(t *testing.T) {
	src := "function a(req) {\n  const q = req.query.q\n}\nfunction b(db) {\n  db.query('SELECT ' + q)\n}\n" +
		"function c(req, db) {\n  if (x) {\n    const v = req.body.v\n  }\n  db.query('SELECT ' + v)\n}\n"
	assert.Equal(t, taintFound(JavaScript, src), []string(nil), "a name does not outlive its block")
}

func TestTaintSupersedesPatternRules(t *testing.T) {
	src := "@app.route('/x')\ndef v():\n    cursor.execute(f\"SELECT * FROM t WHERE n = '{request.args['n']}'\")\n" +
		"    cursor.execute(f\"SELECT * FROM t WHERE n = '{other}'\")\n"
	var got []string
	for _, f := range parsePython([]byte(src)).findings {
		got = append(got, f.rule)
	}
	assert.Equal(t, got, []string{"sql-concat", "sql-from-request"}, "line 3 is request input, line 4 only built dynamically")
	src = "const fs = require('fs')\napp.get('/', (req, res) => {\n  fs.readFileSync(req.query.f)\n})\n"
	got = nil
	for _, f := range parseJS([]byte(src), false).findings {
		got = append(got, f.rule)
	}
	assert.Equal(t, got, []string{"path-from-request"}, "one finding per rule and line")
}

func TestTaintDetailNamesCallAndSource(t *testing.T) {
	src := "@app.route('/x')\ndef v():\n    name = request.args.get('n')\n    cursor.execute('SELECT ' + name)\n"
	for _, f := range parsePython([]byte(src)).findings {
		if f.rule == "sql-from-request" {
			assert.Equal(t, f.detail, "cursor.execute: the SQL text comes from the incoming request (via request.args)")
			assert.Equal(t, f.line, 4)
			assert.Equal(t, f.owner, "v")
		}
	}
}

// The Python, JavaScript and TypeScript examples of the request-input rules
// are what the taint tracking detects.
func TestTaintRuleExamples(t *testing.T) {
	for _, id := range taintRules {
		r := rules.MustGet(id)
		for _, e := range r.Examples {
			var src func(string) string
			switch e.Language {
			case Python:
				src = func(s string) string { return "@app.route('/x')\ndef view():\n" + indent(s, "    ") + "\n" }
			case JavaScript, TypeScript:
				src = func(s string) string {
					return "const cp = require('child_process')\napp.post('/x', async (req, res) => {\n" + indent(s, "  ") + "\n})\n"
				}
			default:
				continue
			}
			t.Run(id+"/"+e.Language, func(t *testing.T) {
				assert.True(t, slices.Contains(taintFound(e.Language, src(e.Vulnerable)), id), "vulnerable example not detected")
				assert.False(t, slices.Contains(taintFound(e.Language, src(e.Fixed)), id), "fixed example detected")
			})
		}
	}
}

func indent(s, by string) string {
	return by + strings.ReplaceAll(s, "\n", "\n"+by)
}

func TestFormatFields(t *testing.T) {
	assert.Equal(t, formatFields("id = {uid} {{x}} {a[0]}"), "      uid         a[0] ")
	v, _ := maskPy(`q = f"a {b} c" + 'd {e}' # {f}`, "", true)
	assert.Contains(t, v, "b")
	assert.NotContains(t, v, "e")
	assert.NotContains(t, v, "f}")
}

func FuzzTaint(f *testing.F) {
	f.Add("@app.route('/<p>')\ndef v(p):\n    q = f\"{p}\"\n    cursor.execute(q, (\n")
	f.Add("app.get('/', (req, res) => {\n  const {a, b: [c]} = req.query\n  fetch(`${a}`\n})")
	f.Add("x = f'{{'\ny = f\"{\"\n")
	f.Fuzz(func(_ *testing.T, src string) {
		_ = parsePython([]byte(src))
		_ = parseJS([]byte(src), true)
		_ = formatFields(src)
		_ = argSpans(src)
	})
}
