package lang

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// C# on the brace engine: namespaces as packages (file-scoped or blocks),
// classes, records, structs and interfaces, methods and constructors,
// using directives, xUnit/NUnit/MSTest tests, ASP.NET controller and
// minimal API routes, and .NET rules.

// CSharp is the language name of .cs files.
const CSharp = "csharp"

const (
	csAttrs = `(?:\[[^\]]*\]\s*)*`
	csMods  = `(?:(?:public|private|protected|internal|static|sealed|abstract|partial|readonly|ref|unsafe|new|file|virtual|override|async|extern|required)\s+)*`
)

var (
	csType      = regexp.MustCompile(`^\s*` + csAttrs + csMods + `(?:class|interface|struct|enum|record(?:\s+(?:class|struct))?)\s+([A-Za-z_]\w*)`)
	csFunc      = regexp.MustCompile(`^\s*` + csAttrs + csMods + `(?:[\w.]+(?:<[^()]*?>)?(?:\[\])*\??\s+|\([^()]*\)\??\s+)?(?P<name>[A-Za-z_]\w*)\s*(?:<[^()]*?>)?\s*\(`)
	csAttr      = regexp.MustCompile(`^\s*\[\s*[A-Za-z_]`)
	csNamespace = regexp.MustCompile(`^\s*namespace\s+([\w.]+)`)
	csUsing     = regexp.MustCompile(`^\s*(?:global\s+)?using\s+(static\s+)?(?:(\w+)\s*=\s*)?([\w.]+)\s*;`)
	csKeywords  = setOf("if for foreach while switch catch return new throw using lock typeof sizeof nameof default checked unchecked fixed base this await async var get set is as in out ref params when where yield else do try finally case stackalloc")
	csBranches  = []string{"if", "for", "foreach", "while", "case", "catch", "when", "?"}
)

var csharpLang = &braceLang{
	lang: CSharp, typeDecl: csType, funcDecl: csFunc, annotation: csAttr, pkgDecl: csNamespace, blockPkg: true,
	// Top-level statements (Program.cs) are calls of the file, and local
	// functions there are not declarations of a type.
	member: func(line, name string) bool {
		before, _, _ := strings.Cut(line, name+"(")
		before = strings.TrimSpace(before)
		return !strings.Contains(before, "=") && !strings.HasPrefix(before, "return") && !strings.HasSuffix(before, "new") &&
			!strings.HasSuffix(before, ".") && !strings.HasPrefix(before, "await") && !strings.HasPrefix(before, "throw")
	},
	imports: func(raw string, line int) (importRef, bool) {
		m := csUsing.FindStringSubmatch(raw)
		if m == nil {
			return importRef{}, false
		}
		return csUsingRef(m[3], m[1] != "", m[2], line), true
	},
	keywords: csKeywords, branches: csBranches,
	exported: func(decl string) bool {
		return strings.Contains(decl, "public ") || strings.Contains(decl, "protected ")
	},
	testAnnotated: csTestAttr, rules: csRules, routes: csRoutes, lineRoutes: csMinimalRoutes,
	textBlocks: true, verbatim: true,
}

// csUsingRef reads a using directive. "using Acme.Data;" brings a whole
// namespace into scope (no names: link() searches it); "using static
// Acme.Util;" and "using R = Acme.Data.Repo;" name one type.
func csUsingRef(spec string, static bool, alias string, line int) importRef {
	ref := importRef{spec: spec, names: map[string]string{}, line: line}
	if !static && alias == "" {
		return ref
	}
	i := strings.LastIndexByte(spec, '.')
	if i < 0 {
		return ref
	}
	ref.spec = spec[:i]
	local := alias
	if local == "" {
		local = spec[i+1:]
	}
	ref.names[local] = spec[i+1:]
	return ref
}

// csLocalNamespace reports whether a using directive names a namespace of
// the repository, a parent of one (using Acme; with Acme.Api declared), or
// a type declared in one (an alias to a nested type, Acme.Api.Outer).
func csLocalNamespace(byPkg map[string][]*parsedFile, ns string) bool {
	if _, ok := byPkg[ns]; ok {
		return true
	}
	if i := strings.LastIndexByte(ns, '.'); i > 0 {
		if _, ok := byPkg[ns[:i]]; ok {
			return true
		}
	}
	for pkg := range byPkg {
		if strings.HasPrefix(pkg, ns+".") {
			return true
		}
	}
	return false
}

// csAttrNames splits "[HttpGet("{id}"), Authorize]" into its attributes,
// each without brackets and without the Attribute suffix.
func csAttrNames(a string) []string {
	a = strings.TrimSpace(a)
	a = strings.TrimSuffix(strings.TrimPrefix(a, "["), "]")
	if i := strings.Index(a, "]"); i >= 0 && !strings.Contains(a[:i], `"`) {
		a = a[:i] // [Fact] public void X() on one line
	}
	var out []string
	depth, quoted, start := 0, false, 0
	for i, c := range a {
		switch {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, strings.TrimSpace(a[start:i]))
			start = i + 1
		}
	}
	return append(out, strings.TrimSpace(a[start:]))
}

// csAttrName is "HttpGet" for `HttpGet("{id}")` and for `HttpGetAttribute`.
func csAttrName(attr string) string {
	if i := strings.IndexAny(attr, "( "); i >= 0 {
		attr = attr[:i]
	}
	attr = attr[strings.LastIndexByte(attr, '.')+1:]
	return strings.TrimSuffix(attr, "Attribute")
}

func csTestAttr(anns []string) bool {
	for _, a := range anns {
		for _, attr := range csAttrNames(a) {
			switch csAttrName(attr) {
			case "Fact", "Theory", "Test", "TestCase", "TestCaseSource", "TestMethod", "DataTestMethod", "SkippableFact", "SkippableTheory":
				return true
			}
		}
	}
	return false
}

var csVerbAttr = map[string]string{"HttpGet": "GET", "HttpPost": "POST", "HttpPut": "PUT", "HttpDelete": "DELETE", "HttpPatch": "PATCH", "HttpHead": "HEAD", "HttpOptions": "OPTIONS"}

// csRoutes reads ASP.NET Core controller routes: [Route] on the class,
// [HttpGet("{id}")] or [Route] on the action, with [controller] and
// [action] tokens filled in and [Authorize] as a guard.
func csRoutes(typeAnns, fnAnns []string, handler string, line int) []route {
	class, action := "", lastPart(handler)
	if i := strings.LastIndexByte(handler, '.'); i >= 0 {
		class = strings.TrimSuffix(lastPart(handler[:i]), "Controller")
	}
	prefix := ""
	var guards []string
	for _, a := range typeAnns {
		for _, attr := range csAttrNames(a) {
			switch csAttrName(attr) {
			case "Route":
				prefix = annPath(attr)
			case "Authorize", "AllowAnonymous":
				guards = append(guards, clipGuard(attr))
			}
		}
	}
	var methods []string
	path, mapped := "", false
	for _, a := range fnAnns {
		for _, attr := range csAttrNames(a) {
			name := csAttrName(attr)
			switch {
			case csVerbAttr[name] != "":
				methods = append(methods, csVerbAttr[name])
				if p := annPath(attr); p != "" {
					path = p
				}
				mapped = true
			case name == "Route":
				path, mapped = annPath(attr), true
			case name == "Authorize" || name == "AllowAnonymous":
				guards = append(guards, clipGuard(attr))
			}
		}
	}
	if !mapped {
		return nil
	}
	if len(methods) == 0 {
		methods = []string{""}
	}
	full := strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(path, "/")
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~/") {
		full = strings.TrimPrefix(path, "~") // an absolute action route ignores the controller's
	}
	full = "/" + strings.Trim(full, "/")
	full = strings.NewReplacer("[controller]", strings.ToLower(class), "[action]", strings.ToLower(action)).Replace(full)
	var out []route
	for _, m := range methods {
		out = append(out, route{kind: model.RouteHTTP, name: strings.TrimSpace(m + " " + full), line: line, handler: handler, guards: guards})
	}
	return out
}

var csMapRoute = regexp.MustCompile(`\.Map(Get|Post|Put|Delete|Patch)\s*\(\s*@?"([^"]*)"\s*,\s*(?:([A-Za-z_][\w.]*)\s*[,)])?`)

// csMinimalRoutes reads minimal API routes: app.MapGet("/x", Handler).
func csMinimalRoutes(raw string, line int) []route {
	m := csMapRoute.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	r := route{kind: model.RouteHTTP, name: strings.ToUpper(m[1]) + " /" + strings.TrimPrefix(m[2], "/"), line: line, handler: m[3]}
	if i := strings.LastIndexByte(m[3], '.'); i > 0 { // Handlers.List
		r.recv, r.handler = m[3][:i], m[3][i+1:]
	}
	for _, g := range []string{"RequireAuthorization", "AllowAnonymous"} {
		if strings.Contains(raw, "."+g+"(") {
			r.guards = append(r.guards, g)
		}
	}
	return []route{r}
}

var (
	csProcess     = regexp.MustCompile(`\bProcess\s*\.\s*Start\s*\(|\bnew\s+ProcessStartInfo\s*\(|\bFileName\s*=`)
	csShell       = regexp.MustCompile(`"(?:/bin/)?(?:sh|bash|zsh|cmd(?:\.exe)?|powershell(?:\.exe)?|pwsh)"`)
	csStartVar    = regexp.MustCompile(`\bProcess\s*\.\s*Start\s*\(\s*[A-Za-z_][\w.]*\s*[,)]`)
	csSQLCall     = regexp.MustCompile(`\bnew\s+(?:Sql|Npgsql|MySql|Sqlite|Oracle|Odbc|OleDb)Command\s*\(|\.\s*(?:FromSqlRaw|ExecuteSqlRaw|ExecuteSqlRawAsync|SqlQueryRaw)\s*\(|\bCommandText\s*=|\.\s*(?:Query|QueryAsync|QueryFirst|QueryFirstOrDefault|QuerySingle|QueryFirstOrDefaultAsync|QuerySingleAsync|Execute|ExecuteAsync|ExecuteScalar|ExecuteScalarAsync)\s*(?:<[^>]*>)?\s*\(`)
	csSQLWords    = jvmSQLWords
	csConcat      = regexp.MustCompile(`"\s*\+\s*[A-Za-z_(]|[A-Za-z_)\]]\s*\+\s*@?"|\$@?"[^"]*\{|@\$"[^"]*\{|string\s*\.\s*(?:Format|Concat)\s*\(|String\s*\.\s*(?:Format|Concat)\s*\(`)
	csDeserialize = regexp.MustCompile(`\bBinaryFormatter\b|\bNetDataContractSerializer\b|\bLosFormatter\b|\bSoapFormatter\b|\bObjectStateFormatter\b|TypeNameHandling\s*=\s*TypeNameHandling\s*\.\s*(?:All|Auto|Objects|Arrays)|new\s+JavaScriptSerializer\s*\(\s*new\s+SimpleTypeResolver`)
	csWeakHash    = regexp.MustCompile(`\b(?:MD5|SHA1)\s*\.\s*(?:Create|HashData|HashDataAsync)\s*\(|\bnew\s+(?:MD5CryptoServiceProvider|SHA1CryptoServiceProvider|SHA1Managed)\s*\(`)
	csWeakCipher  = regexp.MustCompile(`\b(?:DES|TripleDES|RC2)\s*\.\s*Create\s*\(|\bnew\s+(?:DES|TripleDES|RC2)CryptoServiceProvider\s*\(|CipherMode\s*\.\s*ECB`)
	csTrustAll    = regexp.MustCompile(`(?:ServerCertificateCustomValidationCallback|ServerCertificateValidationCallback|RemoteCertificateValidationCallback)\s*(?:=|\+=)\s*(?:\([^)]*\)|\w+)\s*=>\s*true|DangerousAcceptAnyServerCertificateValidator`)
	csRandom      = regexp.MustCompile(`\bnew\s+Random\s*\(|\bRandom\s*\.\s*Shared\b`)
	csSecret      = regexp.MustCompile(`(?:string|var)\s+([A-Za-z_]\w*)\s*=\s*@?"([^"]{8,})"`)
)

// csRules reports C# findings on one line (rules/code/*.yaml).
func csRules(raw, masked string, line int, owner string) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	switch {
	case csProcess.MatchString(masked) && csShell.MatchString(raw) && (strings.Contains(raw, `"/c`) || strings.Contains(raw, `"-c`) || strings.Contains(raw, `"-Command`) || strings.Contains(raw, `$"`)):
		if csConcat.MatchString(raw) {
			add("exec-shell", "a shell runs a command built at run time")
		} else {
			add("exec-variable", "a command runs through a shell")
		}
	case csStartVar.MatchString(masked) || strings.Contains(masked, "Process.Start(") && csConcat.MatchString(raw):
		add("exec-variable", "Process.Start with a program that is not a literal")
	}
	if csSQLCall.MatchString(masked) && csConcat.MatchString(raw) && csSQLWords.MatchString(raw) {
		add("sql-concat", "a query built by concatenation or interpolation")
	}
	if csDeserialize.MatchString(masked) {
		add("deserialize", "a .NET serializer that reads types from the data")
	}
	if csWeakHash.MatchString(masked) {
		add("weak-hash", "MD5 or SHA-1")
	}
	if csWeakCipher.MatchString(masked) {
		add("weak-cipher", "a broken cipher or ECB mode")
	}
	if csTrustAll.MatchString(masked) {
		add("tls-insecure", "certificate validation disabled")
	}
	if csRandom.MatchString(masked) && jvmSecretWord.MatchString(raw) {
		add("insecure-rand", "System.Random for a secret value; use RandomNumberGenerator")
	}
	if m := csSecret.FindStringSubmatch(raw); m != nil && looksSecret(m[1], m[2]) {
		add("hardcoded-secret", fmt.Sprintf("%s is assigned a %d-character literal", m[1], len(m[2])))
	}
	return out
}

// csTags maps .NET library calls to security tags.
func csTags(recv, n string, set map[string]bool) {
	switch {
	case recv == "Process" && n == "Start", n == "ProcessStartInfo":
		set["exec"] = true
	case strings.HasSuffix(n, "Command") && (strings.HasPrefix(n, "Sql") || strings.HasPrefix(n, "Npgsql") || strings.HasPrefix(n, "MySql") || strings.HasPrefix(n, "Sqlite")),
		n == "FromSqlRaw" || n == "ExecuteSqlRaw" || n == "ExecuteSqlRawAsync" || n == "FromSqlInterpolated" || n == "ExecuteReader" || n == "ExecuteNonQuery":
		set["sql"] = true
	case n == "HttpClient" || recv == "httpClient" || recv == "_httpClient" || recv == "client" && (strings.HasPrefix(n, "Get") || strings.HasPrefix(n, "Post") || n == "SendAsync"):
		set["http"] = true
	case recv == "File" || recv == "Directory" || n == "FileStream" || n == "StreamWriter" || n == "StreamReader":
		set["filesystem"] = true
	case recv == "Environment" && (n == "GetEnvironmentVariable" || n == "GetEnvironmentVariables"):
		set["env"] = true
	case recv == "SHA256" || recv == "SHA512" || recv == "Aes" || recv == "AesGcm" || recv == "RSA" || recv == "HMACSHA256" || recv == "RandomNumberGenerator" || n == "HMACSHA256" || recv == "Rfc2898DeriveBytes":
		set["crypto"] = true
	case recv == "JsonConvert" && strings.HasPrefix(n, "Deserialize"), n == "BinaryFormatter", recv == "XmlSerializer" || n == "XmlSerializer":
		set["deserialize"] = true
	}
}
