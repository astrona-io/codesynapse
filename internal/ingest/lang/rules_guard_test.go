package lang

import (
	"strings"
	"testing"
	"unicode"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

var guardedRules = map[string]literalRe{
	"pySubprocess": pySubprocess, "pyShellTrue": pyShellTrue, "pyOsSystem": pyOsSystem, "pyEval": pyEval,
	"pyPickle": pyPickle, "pyYamlLoad": pyYamlLoad, "pyVerify": pyVerify, "pyWeakHash": pyWeakHash,
	"pyExecute": pyExecute, "pyDebug": pyDebug, "pyMktemp": pyMktemp, "pyJWT": pyJWT,
	"pyRandSecret": pyRandSecret, "pyAssign": pyAssign, "pyKwSecret": pyKwSecret,
	"jsEval": jsEval, "jsExec": jsExec, "jsSpawnShell": jsSpawnShell, "jsInnerHTML": jsInnerHTML,
	"jsInsertHTML": jsInsertHTML, "jsWeakHash": jsWeakHash, "jsTLS": jsTLS, "jsSQLTemplate": jsSQLTemplate,
	"jsSQLConcat": jsSQLConcat, "jsFsReq": jsFsReq, "jsRedirect": jsRedirect, "jsJWT": jsJWT,
	"jsRandSecret": jsRandSecret, "jsSecret": jsSecret,
}

// guardSeeds trigger at least one rule each, in several letter cases.
var guardSeeds = []string{
	`subprocess.run(cmd, shell=True)`, `os.system("ls " + d)`, `eval(x)`, `exec(code)`, `pickle.loads(b)`,
	`shelve.open(p)`, `yaml.load(s)`, `requests.get(u, verify=False)`, `ssl._create_unverified_context()`,
	`ctx.verify_mode = ssl.CERT_NONE`, `hashlib.md5(b)`, `hashlib.new("sha1")`, `cur.execute(f"select {x}")`,
	`db.raw("select " + x)`, `app.run(debug=True)`, `DEBUG = True`, `tempfile.mktemp()`,
	`jwt.decode(t, verify=False)`, `TOKEN = random.choice(chars)`, `token = RANDOM.RANDOM()`,
	`api_key = "abcdefgh12345678"`, `connect(password="hunter2hunter2")`,
	`eval(s)`, `new Function(body)`, `execSync(cmd)`, `spawn(c, { shell: true })`, `el.innerHTML = html`,
	`el.insertAdjacentHTML("x", h)`, `document.write(h)`, `createHash('md5')`, `{ rejectUnauthorized: false }`,
	"process.env.NODE_TLS_REJECT_UNAUTHORIZED", "db.query(`select ${id}`)", "DB.QUERY(`SELECT ${ID}`)",
	`db.query("select * from t where id=" + id)`, `DB.Query('SELECT 1' + x)`, `readFile(req.query.p)`,
	`res.redirect(req.query.next)`, `jwt.decode(t)`, `const token = Math.random()`, `const TOKEN = MATH.RANDOM()`,
	`const apiKey = "abcdefgh12345678"`,
}

func checkGuards(t *testing.T, line string) {
	t.Helper()
	for name, r := range guardedRules {
		if r.re.MatchString(line) && !r.mayMatch(line) {
			t.Errorf("%s matches %q but its literals %q skip it", name, line, r.literals)
		}
	}
}

func TestRuleGuards(t *testing.T) {
	for name, r := range guardedRules {
		if strings.HasPrefix(r.re.String(), "(?i)") {
			for _, l := range r.literals {
				assert.False(t, strings.ContainsFunc(l, unicode.IsLetter), name+": a case-insensitive expression needs letter-free literals")
			}
		}
	}
	for _, line := range guardSeeds {
		matched := false
		for _, r := range guardedRules {
			matched = matched || r.MatchString(line)
		}
		assert.True(t, matched, "seed triggers no rule: "+line)
		checkGuards(t, line)
	}
}

func FuzzRuleGuards(f *testing.F) {
	for _, line := range guardSeeds {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) { checkGuards(t, line) })
}
