package lang

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/redact"
	"github.com/parisnakitakejser/codesynapse/rules"
)

// RuleIDs are the rules this detector reports; see rules/code/<id>.yaml.
// IDs shared with the Go detector mean the same thing.
var RuleIDs = []string{
	"exec-shell",
	"exec-variable",
	"code-injection",
	"sql-concat",
	"deserialize",
	"tls-insecure",
	"weak-hash",
	"hardcoded-secret",
	"dom-xss",
	"path-from-request",
	"open-redirect",
	"jwt-unverified",
	"insecure-rand",
	"debug-enabled",
	"insecure-temp",
	"vue-v-html",
}

func (ix *indexer) report(pf *parsedFile, owner string, f finding) {
	r := rules.MustGet(f.rule)
	id := ix.id(model.KindFinding, fmt.Sprintf("%s:%d:%s", pf.rel, f.line, f.rule))
	ix.g.AddNode(model.Node{
		ID: id, Kind: model.KindFinding, Repo: ix.repo, Name: r.Title, Rule: r.ID, Severity: r.Severity, CWE: r.CWE,
		File: pf.rel, StartLine: f.line, EndLine: f.line, Summary: redact.Summary(f.detail+". "+r.Fix, 300), Language: pf.lang,
	})
	ix.g.AddEdge(model.Edge{From: owner, To: id, Kind: model.EdgeHasFinding})
}

var (
	secretNameRe = regexp.MustCompile(`(?i)(passw(or)?d|passwd|secret|token|api[_-]?key|apikey|private[_-]?key|access[_-]?key|credential)`)
	placeholder  = regexp.MustCompile(`(?i)^(x+|\*+|changeme|change-me.*|example.*|dummy|test|password|secret|token|redacted|todo|none|null|<.*>|\$\{.*\}|your[-_].*)$`)

	pySubprocess = regexp.MustCompile(`\bsubprocess\.(run|call|Popen|check_output|check_call)\s*\(`)
	pyShellTrue  = regexp.MustCompile(`shell\s*=\s*True`)
	pyOsSystem   = regexp.MustCompile(`\bos\.(system|popen)\s*\(`)
	pyEval       = regexp.MustCompile(`(^|[^\w.])(eval|exec)\s*\(`)
	pyPickle     = regexp.MustCompile(`\b(pickle|cPickle|dill|marshal|joblib)\.loads?\s*\(|\bshelve\.open\s*\(`)
	pyYamlLoad   = regexp.MustCompile(`\byaml\.(load|load_all|unsafe_load)\s*\(`)
	pyVerify     = regexp.MustCompile(`verify\s*=\s*False|_create_unverified_context|CERT_NONE`)
	pyWeakHash   = regexp.MustCompile(`\bhashlib\.(md5|sha1)\s*\(|hashlib\.new\(\s*['"](md5|sha1)['"]`)
	pyExecute    = regexp.MustCompile(`\.(execute|executemany|raw|executescript)\s*\(\s*(f["']|["'][^"']*["']\s*(%|\.format|\+))`)
	pyDebug      = regexp.MustCompile(`\.run\([^)]*debug\s*=\s*True|DEBUG\s*=\s*True`)
	pyMktemp     = regexp.MustCompile(`\btempfile\.mktemp\s*\(`)
	pyJWT        = regexp.MustCompile(`\bjwt\.decode\s*\(.*(verify\s*=\s*False|verify_signature["']?\s*:\s*False)`)
	pyRandSecret = regexp.MustCompile(`(?i)\b\w*(token|secret|password|nonce|salt|key)\w*\s*=.*\brandom\.(random|randint|choice|choices|getrandbits)\s*\(`)
	pyAssign     = regexp.MustCompile(`^\s*([A-Za-z_][\w.]*)\s*(:\s*\w+)?\s*=\s*['"]([^'"]{8,})['"]\s*$`)
	pyKwSecret   = regexp.MustCompile(`\b([A-Za-z_]\w*)\s*=\s*['"]([^'"]{8,})['"]`)
)

func looksSecret(name, value string) bool {
	if !secretNameRe.MatchString(name) || placeholder.MatchString(value) || strings.ContainsAny(value, " /{}") {
		return false
	}
	lower := strings.ToLower(name)
	for _, ref := range []string{"name", "key_id", "file", "path", "env", "url", "type", "header", "field", "prefix", "_ref"} {
		if strings.HasSuffix(lower, ref) {
			return false
		}
	}
	upper := true
	for _, c := range value {
		if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			upper = false
		}
	}
	return !upper // ENV_VAR_NAMES are references, not secrets
}

func pyRules(raw, masked string, line int, owner string) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	dynamic := strings.Contains(raw, `f"`) || strings.Contains(raw, `f'`) || strings.Contains(raw, ".format(") || strings.Contains(raw, "%") || strings.Contains(raw, "+")
	switch {
	case pySubprocess.MatchString(masked) && pyShellTrue.MatchString(raw):
		if dynamic {
			add("exec-shell", "subprocess with shell=True and a formatted command")
		} else {
			add("exec-variable", "subprocess with shell=True")
		}
	case pyOsSystem.MatchString(masked):
		if dynamic || !strings.Contains(raw, `("`) && !strings.Contains(raw, `('`) {
			add("exec-shell", "os.system/os.popen with a non-literal command")
		} else {
			add("exec-variable", "os.system/os.popen runs through a shell")
		}
	}
	if pyEval.MatchString(masked) && !strings.Contains(masked, "literal_eval") {
		add("code-injection", "eval/exec")
	}
	if pyPickle.MatchString(masked) {
		add("deserialize", "pickle-style deserialisation")
	}
	if pyYamlLoad.MatchString(masked) && !strings.Contains(raw, "SafeLoader") && !strings.Contains(raw, "CSafeLoader") {
		add("deserialize", "yaml.load without SafeLoader")
	}
	if pyVerify.MatchString(raw) {
		add("tls-insecure", "certificate verification disabled")
	}
	if pyWeakHash.MatchString(raw) && !strings.Contains(raw, "usedforsecurity=False") {
		add("weak-hash", "hashlib md5/sha1")
	}
	if pyExecute.MatchString(raw) {
		add("sql-concat", "execute() with a formatted query")
	}
	if pyDebug.MatchString(raw) {
		add("debug-enabled", "debug=True")
	}
	if pyMktemp.MatchString(masked) {
		add("insecure-temp", "tempfile.mktemp")
	}
	if pyJWT.MatchString(raw) {
		add("jwt-unverified", "jwt.decode without verification")
	}
	if pyRandSecret.MatchString(raw) {
		add("insecure-rand", "random module used for a secret value")
	}
	if m := pyAssign.FindStringSubmatch(raw); m != nil && looksSecret(m[1], m[3]) {
		add("hardcoded-secret", fmt.Sprintf("%s is assigned a %d-character literal", m[1], len(m[3])))
	} else {
		for _, m := range pyKwSecret.FindAllStringSubmatch(raw, -1) {
			if looksSecret(m[1], m[2]) {
				add("hardcoded-secret", fmt.Sprintf("%s= receives a %d-character literal", m[1], len(m[2])))
				break
			}
		}
	}
	return out
}

var (
	jsEval        = regexp.MustCompile(`(^|[^\w.$])eval\s*\(|\bnew\s+Function\s*\(`)
	jsExec        = regexp.MustCompile(`\b(exec|execSync)\s*\(`)
	jsSpawnShell  = regexp.MustCompile(`\b(spawn|spawnSync|execFile|execFileSync)\s*\(.*shell\s*:\s*true`)
	jsInnerHTML   = regexp.MustCompile(`\.(innerHTML|outerHTML)\s*\+?=\s*[^'"\s;]`)
	jsInsertHTML  = regexp.MustCompile(`\.insertAdjacentHTML\s*\(|\bdocument\.write(ln)?\s*\(|dangerouslySetInnerHTML`)
	jsWeakHash    = regexp.MustCompile(`createHash\(\s*['"](md5|sha1)['"]`)
	jsTLS         = regexp.MustCompile(`rejectUnauthorized\s*:\s*false|NODE_TLS_REJECT_UNAUTHORIZED`)
	jsSQLTemplate = regexp.MustCompile("(?i)\\.(query|execute|raw|unsafe|prepare)\\s*\\(\\s*`[^`]*\\$\\{")
	jsSQLConcat   = regexp.MustCompile(`(?i)\.(query|execute|raw)\s*\(\s*['"]\s*(select|insert|update|delete)\b[^'"]*['"]\s*\+`)
	jsFsReq       = regexp.MustCompile(`\b(readFile|readFileSync|createReadStream|writeFile|writeFileSync|unlink|unlinkSync|sendFile|download)\s*\(.*\breq\.(query|params|body)`)
	jsRedirect    = regexp.MustCompile(`\bredirect\s*\(.*\breq\.(query|params|body)`)
	jsJWT         = regexp.MustCompile(`\bjwt\.decode\s*\(`)
	jsRandSecret  = regexp.MustCompile(`(?i)\b\w*(token|secret|password|nonce|salt|key)\w*\s*[=:].*Math\.random\s*\(`)
	jsSecret      = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*[:=]\s*['"]([^'"]{8,})['"]`)
)

func jsRules(raw, masked string, line int, owner string, childProcess bool) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	if jsEval.MatchString(masked) {
		add("code-injection", "eval / new Function")
	}
	if childProcess && jsExec.MatchString(masked) {
		if strings.Contains(raw, "${") || strings.Contains(masked, "+") {
			add("exec-shell", "child_process exec with an interpolated command")
		} else {
			add("exec-variable", "child_process exec runs through a shell")
		}
	}
	if jsSpawnShell.MatchString(raw) {
		add("exec-variable", "spawn with shell: true")
	}
	if jsInnerHTML.MatchString(masked) {
		add("dom-xss", "innerHTML/outerHTML assigned a dynamic value")
	}
	if jsInsertHTML.MatchString(masked) {
		add("dom-xss", "raw HTML insertion")
	}
	if jsWeakHash.MatchString(raw) {
		add("weak-hash", "createHash md5/sha1")
	}
	if jsTLS.MatchString(raw) {
		add("tls-insecure", "TLS verification disabled")
	}
	if jsSQLTemplate.MatchString(raw) || jsSQLConcat.MatchString(raw) {
		add("sql-concat", "query built with interpolation or concatenation")
	}
	if jsFsReq.MatchString(raw) {
		add("path-from-request", "filesystem call with request input")
	}
	if jsRedirect.MatchString(raw) {
		add("open-redirect", "redirect to request input")
	}
	if jsJWT.MatchString(masked) && !strings.Contains(raw, "verify") {
		add("jwt-unverified", "jwt.decode does not verify signatures")
	}
	if jsRandSecret.MatchString(raw) {
		add("insecure-rand", "Math.random used for a secret value")
	}
	for _, m := range jsSecret.FindAllStringSubmatch(raw, -1) {
		if looksSecret(m[1], m[2]) {
			add("hardcoded-secret", fmt.Sprintf("%s holds a %d-character literal", m[1], len(m[2])))
			break
		}
	}
	return out
}

var vHTML = regexp.MustCompile(`\bv-html\s*=`)

func vueRules(src []byte) []finding {
	var out []finding
	inTemplate := false
	for i, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "<template") {
			inTemplate = true
		}
		if strings.Contains(l, "<script") {
			inTemplate = false
		}
		if inTemplate && vHTML.MatchString(l) {
			out = append(out, finding{rule: "vue-v-html", detail: "v-html binding", line: i + 1})
		}
	}
	return out
}

// tagsFor derives the security tags of a function from what it calls.
func tagsFor(calls []call, lang string) []string {
	set := map[string]bool{}
	for _, c := range calls {
		r, n := strings.ToLower(c.recv), c.name
		switch {
		case r == "subprocess" || (r == "os" && (n == "system" || n == "popen" || strings.HasPrefix(n, "exec") || strings.HasPrefix(n, "spawn"))):
			set["exec"] = true
		case r == "child_process" || ((n == "exec" || n == "execSync" || n == "spawn" || n == "spawnSync" || n == "execFile") && lang != Python):
			set["exec"] = true
		case n == "eval" || (r == "" && n == "exec" && lang == Python):
			set["exec"] = true
		case r == "pickle" || r == "marshal" || r == "dill" || (r == "yaml" && n == "load"):
			set["deserialize"] = true
		case r == "requests" || r == "httpx" || r == "urllib" || r == "aiohttp" || r == "axios" || n == "fetch" || r == "http" || r == "https" || n == "urlopen":
			set["http"] = true
		case r == "hashlib" || r == "crypto" || r == "hmac" || r == "secrets" || r == "bcrypt" || r == "jwt":
			set["crypto"] = true
		case r == "fs" || r == "fsp" || r == "shutil" || (r == "" && n == "open" && lang == Python) || (r == "os" && (n == "remove" || n == "unlink" || n == "chmod" || n == "makedirs")):
			set["filesystem"] = true
		case n == "execute" || n == "executemany" || n == "query" && (r == "db" || r == "pool" || r == "client" || r == "conn" || r == "connection" || r == "knex" || r == "sql"):
			set["sql"] = true
		case r == "os" && (n == "getenv" || n == "environ"), r == "process" && n == "env":
			set["env"] = true
		}
	}
	var out []string
	for _, t := range []string{"exec", "sql", "http", "filesystem", "crypto", "deserialize", "env"} {
		if set[t] {
			out = append(out, t)
		}
	}
	return out
}
