package lang

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// PHP on the brace engine: namespaces as packages, classes, interfaces,
// traits and enums, methods and functions, use imports, PHPUnit and Pest
// tests, Laravel, Symfony and Slim routes, and PHP rules. Text outside
// <?php … ?> (templates) is masked like a string.

// PHP is the language name of .php files.
const PHP = "php"

const phpAttrs = `(?:#\[[^\]]*\]\s*)*`

var (
	phpType      = regexp.MustCompile(`^\s*` + phpAttrs + `(?:(?:abstract|final|readonly)\s+)*(?:class|interface|trait|enum)\s+([A-Za-z_]\w*)`)
	phpFunc      = regexp.MustCompile(`^\s*` + phpAttrs + `(?:(?:public|protected|private|static|abstract|final)\s+)*function\s+&?(?P<name>[A-Za-z_]\w*)\s*\(`)
	phpAttr      = regexp.MustCompile(`^\s*#\[`)
	phpNamespace = regexp.MustCompile(`^\s*namespace\s+\\?([\w\\]+)`)
	phpUse       = regexp.MustCompile(`^\s*use\s+(?:(function|const)\s+)?\\?([^;]+);`)
	phpKeywords  = setOf("if elseif else for foreach while switch match catch return new throw echo print isset unset empty list array fn function use include require include_once require_once declare exit die clone instanceof and or xor")
	phpBranches  = []string{"if", "elseif", "for", "foreach", "while", "case", "catch", "?", "=>"}
)

var phpLang = &braceLang{
	lang: PHP, typeDecl: phpType, funcDecl: phpFunc, annotation: phpAttr, pkgDecl: phpNamespace, blockPkg: true, topFuncs: true, php: true,
	imports: func(raw string, line int) (importRef, bool) {
		m := phpUse.FindStringSubmatch(raw)
		if m == nil {
			return importRef{}, false
		}
		return phpUseRef(m[2], line), true
	},
	keywords: phpKeywords, branches: phpBranches,
	exported: func(decl string) bool {
		return !strings.Contains(decl, "private ") && !strings.Contains(decl, "protected ")
	},
	testAnnotated: phpTestAttr, rules: phpRules, routes: phpAttrRoutes, lineRoutes: phpLineRoutes,
}

// phpUseRef reads "App\Models\User", "App\Models\User as U" and the group
// form "App\Models\{User, Post as P}" into an import of namespace App\Models.
func phpUseRef(spec string, line int) importRef {
	ref := importRef{names: map[string]string{}, line: line}
	spec = strings.TrimSpace(spec)
	if open := strings.Index(spec, "{"); open >= 0 {
		ref.spec = strings.Trim(strings.TrimSpace(spec[:open]), `\`)
		for _, item := range strings.Split(strings.TrimSuffix(strings.TrimSpace(spec[open+1:]), "}"), ",") {
			name, alias, ok := strings.Cut(strings.TrimSpace(item), " as ")
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			name = name[strings.LastIndexByte(name, '\\')+1:] // App\{Models\User}: the leaf
			local := name
			if ok {
				local = strings.TrimSpace(alias)
			}
			ref.names[local] = name
		}
		return ref
	}
	name, alias, ok := strings.Cut(spec, " as ")
	name = strings.TrimSpace(name)
	i := strings.LastIndexByte(name, '\\')
	if i < 0 {
		ref.spec = name // a global class: use Exception;
		return ref
	}
	ref.spec, name = name[:i], name[i+1:]
	local := name
	if ok {
		local = strings.TrimSpace(alias)
	}
	ref.names[local] = name
	return ref
}

func phpTestAttr(anns []string) bool {
	for _, a := range anns {
		if n := phpAttrName(a); n == "Test" || n == "DataProvider" || n == "TestWith" {
			return true
		}
	}
	return false
}

// phpAttrName is "Route" for `#[Route('/x')]` and for `#[Symfony\…\Route]`.
func phpAttrName(a string) string {
	a = strings.TrimPrefix(strings.TrimSpace(a), "#[")
	if i := strings.IndexAny(a, "( ]"); i >= 0 {
		a = a[:i]
	}
	return a[strings.LastIndexByte(a, '\\')+1:]
}

var (
	phpString     = regexp.MustCompile(`['"]([^'"]*)['"]`)
	phpAttrVerbs  = regexp.MustCompile(`methods\s*:\s*\[?([^\]\)]*)`)
	phpVerbWord   = regexp.MustCompile(`(?i)\b(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\b`)
	phpVerbAttr   = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE"}
	phpGuardAttr  = map[string]bool{"IsGranted": true, "Security": true, "Middleware": true}
	laravelRoute  = regexp.MustCompile(`\bRoute::(get|post|put|patch|delete|options|any)\s*\(\s*['"]([^'"]*)['"]`)
	slimRoute     = regexp.MustCompile(`\$(?:app|group|router|r|route)\s*->\s*(get|post|put|patch|delete|options|any)\s*\(\s*['"]([^'"]*)['"]`)
	phpHandler    = regexp.MustCompile(`\[\s*\\?([\w\\]+)::class\s*,\s*['"](\w+)['"]\s*\]|['"]\\?([\w\\]+)@(\w+)['"]|,\s*\\?([\w\\]+)::class\s*\)`)
	phpMiddleware = regexp.MustCompile(`->\s*middleware\s*\(\s*\[?\s*['"]([^'"]+)['"]`)
)

// phpAttrRoutes reads Symfony's #[Route('/x', methods: ['GET'])] (with the
// class's #[Route] as prefix) and route attributes such as #[Get('/x')].
func phpAttrRoutes(typeAnns, fnAnns []string, handler string, line int) []route {
	prefix := ""
	var guards []string
	for _, a := range typeAnns {
		switch n := phpAttrName(a); {
		case n == "Route":
			if m := phpString.FindStringSubmatch(a); m != nil {
				prefix = m[1]
			}
		case phpGuardAttr[n]:
			guards = append(guards, clipGuard(strings.TrimPrefix(strings.TrimSuffix(a, "]"), "#[")))
		}
	}
	var methods []string
	path, mapped := "", false
	for _, a := range fnAnns {
		n := phpAttrName(a)
		switch {
		case n == "Route":
			if m := phpString.FindStringSubmatch(a); m != nil {
				path = m[1]
			}
			if m := phpAttrVerbs.FindStringSubmatch(a); m != nil {
				for _, v := range phpVerbWord.FindAllString(m[1], -1) {
					methods = append(methods, strings.ToUpper(v))
				}
			}
			mapped = true
		case phpVerbAttr[n] != "":
			methods = append(methods, phpVerbAttr[n])
			if m := phpString.FindStringSubmatch(a); m != nil {
				path = m[1]
			}
			mapped = true
		case phpGuardAttr[n]:
			guards = append(guards, clipGuard(strings.TrimPrefix(strings.TrimSuffix(a, "]"), "#[")))
		}
	}
	if !mapped {
		return nil
	}
	if len(methods) == 0 {
		methods = []string{""}
	}
	full := "/" + strings.Trim(strings.TrimSuffix(prefix, "/")+"/"+strings.TrimPrefix(path, "/"), "/")
	var out []route
	for _, m := range methods {
		out = append(out, route{kind: model.RouteHTTP, name: strings.TrimSpace(m + " " + full), line: line, handler: handler, guards: guards})
	}
	return out
}

// phpLineRoutes reads Laravel's Route::get('/x', …) and Slim's
// $app->get('/x', …), with ->middleware('auth') on the line as a guard.
func phpLineRoutes(raw string, line int) []route {
	m := laravelRoute.FindStringSubmatch(raw)
	if m == nil {
		m = slimRoute.FindStringSubmatch(raw)
	}
	if m == nil {
		return nil
	}
	verb := strings.ToUpper(m[1])
	if verb == "ANY" {
		verb = ""
	}
	r := route{kind: model.RouteHTTP, name: strings.TrimSpace(verb + " /" + strings.TrimPrefix(m[2], "/")), line: line}
	// [UserController::class, 'show'], 'UserController@show', or an
	// invokable UserController::class.
	if h := phpHandler.FindStringSubmatch(raw); h != nil {
		cls, method := h[1]+h[3]+h[5], h[2]+h[4]
		if method == "" {
			method = "__invoke"
		}
		r.recv, r.handler = cls[strings.LastIndexByte(cls, '\\')+1:], method
	}
	if g := phpMiddleware.FindStringSubmatch(raw); g != nil {
		r.guards = []string{"middleware(" + g[1] + ")"}
	}
	return []route{r}
}

var (
	phpShellFn  = regexp.MustCompile(`(?:^|[^\w$>:])(?:shell_exec|exec|system|passthru|popen|proc_open|pcntl_exec)\s*\(`)
	phpBacktick = regexp.MustCompile("`[^`]*\\$[^`]*`")
	// phpBuilt is a string built at run time: interpolated ("…$x…"),
	// concatenated ('…' . $x, $x . '…') or formatted.
	phpBuilt  = regexp.MustCompile(`"[^"]*\$[A-Za-z_{][^"]*"|['"]\s*\.\s*\$|\$\w+(?:->\w+|\[[^\]]*\])*\s*\.\s*['"]|\bsprintf\s*\(`)
	phpVarArg = regexp.MustCompile(`\(\s*\$`)
	// phpSQLText is SQL inside a string literal, not a ->where( call.
	phpSQLText    = regexp.MustCompile(`(?i)["'][^"']*\b(?:select|insert|update|delete)\b[^"']*\b(?:from|into|set|where|values)\b|["'][^"']*\bwhere\b`)
	phpSQLRaw     = regexp.MustCompile(`(?:->|::)\s*(?:raw|whereRaw|selectRaw|orderByRaw|havingRaw|fromRaw|statement|unprepared)\s*\(`)
	phpSQLCall    = regexp.MustCompile(`(?:->|::)\s*(?:query|exec|prepare|select|statement|unprepared|raw|whereRaw|selectRaw|orderByRaw|havingRaw|fromRaw)\s*\(\s*[^)\s]|\b(?:mysqli_query|mysqli_multi_query|mysql_query|pg_query|sqlite_query)\s*\(`)
	phpEval       = regexp.MustCompile(`(?:^|[^\w$>:])(?:eval|create_function|assert)\s*\(|preg_replace\s*\(\s*['"][/#~][^'"]*[/#~][a-zA-Z]*e[a-zA-Z]*['"]`) // the /e modifier evaluates the replacement
	phpUnserial   = regexp.MustCompile(`(?:^|[^\w$>:])unserialize\s*\(\s*\$`)
	phpRequest    = regexp.MustCompile(`\$_(?:GET|POST|REQUEST|COOKIE|FILES)\b|\$request\s*->\s*(?:input|get|query|post|file|route)\s*\(|\brequest\s*\(\s*\)\s*->|\brequest\s*\(\s*['"]`)
	phpFileFn     = regexp.MustCompile(`(?:^|[^\w$>:])(?:include|include_once|require|require_once)\b|(?:^|[^\w$>:])(?:file_get_contents|file_put_contents|fopen|readfile|unlink|file|copy|rename|mkdir|rmdir)\s*\(`)
	phpWeakHash   = regexp.MustCompile(`(?:^|[^\w$>:])(?:md5|sha1)\s*\(|\bhash\s*\(\s*['"](?:md5|sha1)['"]`)
	phpWeakCipher = regexp.MustCompile(`(?i)openssl_(?:en|de)crypt\s*\([^)]*['"](?:des|des-ede3|rc2|rc4|bf|blowfish)[-\w]*['"]|openssl_(?:en|de)crypt\s*\([^)]*-ecb['"]|\bMCRYPT_(?:DES|3DES|RC2|BLOWFISH)|\bMCRYPT_MODE_ECB`)
	phpTrustAll   = regexp.MustCompile(`CURLOPT_SSL_VERIFYPEER\s*(?:,|=>)\s*(?:false|0)\b|CURLOPT_SSL_VERIFYHOST\s*(?:,|=>)\s*(?:false|0)\b|['"]verify(?:_peer|_peer_name)?['"]\s*=>\s*false|->\s*withoutVerifying\s*\(`)
	phpRandom     = regexp.MustCompile(`(?:^|[^\w$>:])(?:rand|mt_rand|uniqid|lcg_value|str_shuffle)\s*\(`)
	phpSecret     = regexp.MustCompile(`(?:\$|const\s+)([A-Za-z_]\w*)\s*=\s*['"]([^'"]{8,})['"]`)
)

// phpRules reports PHP findings on one line (rules/code/*.yaml).
func phpRules(raw, masked string, line int, owner string) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	switch {
	case phpShellFn.MatchString(masked) && (phpBuilt.MatchString(raw) || phpVarArg.MatchString(masked)) && !strings.Contains(raw, "escapeshellarg") && !strings.Contains(raw, "escapeshellcmd"):
		add("exec-shell", "a shell runs a command built from variables")
	case phpBacktick.MatchString(raw) && strings.Contains(masked, "`"):
		add("exec-shell", "backticks run a command built from variables")
	}
	if phpSQLCall.MatchString(masked) && phpBuilt.MatchString(raw) && (phpSQLText.MatchString(raw) || phpSQLRaw.MatchString(masked)) {
		add("sql-concat", "a query built by concatenation or interpolation")
	}
	if phpEval.MatchString(masked) && (phpBuilt.MatchString(raw) || phpVarArg.MatchString(masked) || strings.Contains(raw, "preg_replace")) {
		add("code-injection", "eval, assert or create_function with a dynamic string")
	}
	if phpUnserial.MatchString(masked) && !strings.Contains(raw, "allowed_classes") {
		add("deserialize", "unserialize without allowed_classes")
	}
	if phpFileFn.MatchString(masked) && phpRequest.MatchString(raw) {
		add("path-from-request", "include or filesystem call with request input")
	}
	if phpWeakHash.MatchString(masked) && jvmSecretWord.MatchString(raw) {
		add("weak-hash", "MD5 or SHA-1 for a secret value")
	}
	if phpWeakCipher.MatchString(raw) {
		add("weak-cipher", "a broken cipher or ECB mode")
	}
	if phpTrustAll.MatchString(raw) {
		add("tls-insecure", "certificate verification disabled")
	}
	if phpRandom.MatchString(masked) && jvmSecretWord.MatchString(raw) {
		add("insecure-rand", "rand/mt_rand/uniqid for a secret value; use random_bytes")
	}
	if m := phpSecret.FindStringSubmatch(raw); m != nil && looksSecret(m[1], m[2]) {
		add("hardcoded-secret", fmt.Sprintf("%s is assigned a %d-character literal", m[1], len(m[2])))
	}
	return out
}

// phpTags maps PHP library calls to security tags.
func phpTags(recv, n string, set map[string]bool) {
	switch {
	case n == "shell_exec" || n == "system" || n == "passthru" || n == "proc_open" || n == "popen" || recv == "Process":
		set["exec"] = true
	case recv == "DB" || n == "mysqli_query" || n == "pg_query" || recv == "$pdo" || recv == "$db" || n == "whereRaw" || n == "selectRaw":
		set["sql"] = true
	case recv == "Http" || n == "curl_exec" || n == "curl_init" || recv == "$client" && (n == "request" || n == "get" || n == "post"):
		set["http"] = true
	case recv == "Storage" || recv == "File" || n == "file_get_contents" || n == "file_put_contents" || n == "fopen" || n == "unlink":
		set["filesystem"] = true
	case n == "getenv" || n == "env":
		set["env"] = true
	case n == "password_hash" || n == "password_verify" || n == "hash_hmac" || n == "openssl_encrypt" || n == "random_bytes" || recv == "Crypt" || recv == "Hash":
		set["crypto"] = true
	case n == "unserialize":
		set["deserialize"] = true
	}
}

// maskPHP blanks string contents, comments (//, #, /* */), heredocs and
// everything outside <?php … ?>, keeping line structure.
func maskPHP(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	html := true
	for i := 0; i < len(b); i++ {
		c := b[i]
		if html {
			if strings.HasPrefix(src[i:], "<?php") || strings.HasPrefix(src[i:], "<?=") {
				html = false
				n := 5
				if b[i+2] == '=' {
					n = 3
				}
				for k := i; k < i+n; k++ {
					blank(k)
				}
				i += n - 1
				continue
			}
			blank(i)
			continue
		}
		switch {
		case c == '?' && i+1 < len(b) && b[i+1] == '>':
			html = true
			blank(i)
			blank(i + 1)
			i++
		case c == '/' && i+1 < len(b) && b[i+1] == '/', c == '#' && (i+1 >= len(b) || b[i+1] != '['):
			for i < len(b) && b[i] != '\n' && !strings.HasPrefix(src[i:], "?>") {
				blank(i)
				i++
			}
			i--
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			for i < len(b) && (b[i] != '*' || i+1 >= len(b) || b[i+1] != '/') {
				blank(i)
				i++
			}
			if i+1 < len(b) {
				blank(i)
				blank(i + 1)
				i++
			}
		case strings.HasPrefix(src[i:], "<<<"):
			nl := strings.IndexByte(src[i:], '\n')
			if nl < 0 {
				continue
			}
			label := strings.Trim(strings.TrimSpace(src[i+3:i+nl]), `'"`)
			if label == "" {
				continue
			}
			j := i + nl + 1
			for j < len(b) {
				end := strings.IndexByte(src[j:], '\n')
				if end < 0 {
					end = len(src) - j
				}
				if t := strings.TrimLeft(src[j:j+end], " \t"); strings.HasPrefix(t, label) && (len(t) == len(label) || !isIdent(rune(t[len(label)]))) {
					break
				}
				for k := j; k < j+end; k++ {
					blank(k)
				}
				j += end + 1
			}
			i = j - 1
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(b) && b[j] != c {
				if b[j] == '\\' {
					blank(j)
					j++
				}
				if j < len(b) {
					blank(j)
				}
				j++
			}
			i = j
		}
	}
	return string(out)
}
