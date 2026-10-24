package lang

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Java and Kotlin: one braceLang each, sharing the JVM's APIs for security
// tags, rules and routes (Spring, JAX-RS, Micronaut, Ktor).

// JVM languages.
const (
	Java   = "java"
	Kotlin = "kotlin"
)

const jvmAnn = `(?:@[\w.]+(?:\([^)]*\))?\s+)*`

var (
	javaType      = regexp.MustCompile(`^\s*` + jvmAnn + `(?:(?:public|protected|private|static|final|abstract|sealed|non-sealed|strictfp)\s+)*(?:class|interface|enum|record|@interface)\s+([A-Za-z_$][\w$]*)`)
	javaFunc      = regexp.MustCompile(`^\s*` + jvmAnn + `(?:(?:public|protected|private|static|final|abstract|synchronized|native|default|strictfp)\s+)*(?:<[^>]+>\s+)?(?:[\w$.]+(?:<[^()]*?>)?(?:\[\])*\s+)?(?P<name>[A-Za-z_$][\w$]*)\s*\(`)
	kotlinType    = regexp.MustCompile(`^\s*` + jvmAnn + `(?:(?:public|private|internal|protected|open|abstract|sealed|data|enum|annotation|inner|value|fun|expect|actual|final)\s+)*(?:class|interface|object)\s+([A-Za-z_][\w]*)|^\s*(?:(?:private|internal)\s+)?companion\s+object(?:\s+([A-Za-z_]\w*))?`)
	kotlinFunc    = regexp.MustCompile(`^\s*` + jvmAnn + `(?:(?:public|private|internal|protected|open|override|abstract|final|suspend|inline|operator|infix|tailrec|external|actual|expect)\s+)*fun\s+(?:<[^>]+>\s+)?(?:[\w.<>?]+\.)?(?P<name>[A-Za-z_][\w]*)\s*\(`)
	jvmAnnotation = regexp.MustCompile(`^\s*@[A-Za-z_][\w.]*`)
	jvmPackage    = regexp.MustCompile(`^\s*package\s+([\w.]+)`)
	javaImport    = regexp.MustCompile(`^\s*import\s+(static\s+)?([\w.]+?)(\.\*)?\s*;`)
	kotlinImport  = regexp.MustCompile(`^\s*import\s+([\w.]+?)(\.\*)?(?:\s+as\s+(\w+))?\s*$`)
	jvmKeywords   = setOf("if for while switch catch return new throw throws synchronized else do try finally super this assert case when in is as fun val var class object interface typealias import package")
	jvmBranches   = []string{"if", "for", "while", "case", "catch", "when"}
)

var javaLang = &braceLang{
	lang: Java, typeDecl: javaType, funcDecl: javaFunc, annotation: jvmAnnotation, pkgDecl: jvmPackage,
	member: func(line, name string) bool {
		before, _, _ := strings.Cut(line, name+"(")
		before = strings.TrimSpace(before)
		// a field initialiser or a statement, not a declaration
		return !strings.Contains(before, "=") && !strings.HasPrefix(before, "return") && !strings.HasSuffix(before, "new") && !strings.HasSuffix(before, ".")
	},
	imports: func(raw string, line int) (importRef, bool) {
		m := javaImport.FindStringSubmatch(raw)
		if m == nil {
			return importRef{}, false
		}
		return jvmImportRef(m[2], m[1] != "", m[3] != "", "", line), true
	},
	keywords: jvmKeywords, branches: jvmBranches,
	exported:      func(decl string) bool { return strings.Contains(decl, "public") || strings.Contains(decl, "protected") },
	testAnnotated: jvmTestAnnotated, rules: jvmRules, routes: jvmRoutes,
	textBlocks: true,
}

var kotlinLang = &braceLang{
	lang: Kotlin, typeDecl: kotlinType, funcDecl: kotlinFunc, annotation: jvmAnnotation, pkgDecl: jvmPackage, topFuncs: true,
	imports: func(raw string, line int) (importRef, bool) {
		m := kotlinImport.FindStringSubmatch(raw)
		if m == nil {
			return importRef{}, false
		}
		return jvmImportRef(m[1], false, m[2] != "", m[3], line), true
	},
	keywords: jvmKeywords, branches: jvmBranches,
	exported: func(decl string) bool {
		return !strings.Contains(decl, "private ") && !strings.Contains(decl, "internal ")
	},
	testAnnotated: jvmTestAnnotated, rules: jvmRules, routes: jvmRoutes, lineRoutes: ktorRoutes,
	textBlocks: true,
}

// jvmImportRef turns "com.acme.Foo" into an import of Foo from package
// com.acme; a static import names the member through its class (C.m).
func jvmImportRef(path string, static, wildcard bool, alias string, line int) importRef {
	ref := importRef{spec: path, names: map[string]string{}, line: line}
	if wildcard {
		return ref
	}
	parts := strings.Split(path, ".")
	member := parts[len(parts)-1]
	local := member
	if alias != "" {
		local = alias
	}
	if static && len(parts) > 1 {
		ref.spec = strings.Join(parts[:len(parts)-1], ".")
		member = parts[len(parts)-2] + "." + member
	}
	ref.names[local] = member
	return ref
}

func jvmTestAnnotated(anns []string) bool {
	for _, a := range anns {
		switch annName(a) {
		case "Test", "ParameterizedTest", "RepeatedTest", "TestFactory", "TestTemplate", "org.junit.Test", "org.junit.jupiter.api.Test":
			return true
		}
	}
	return false
}

// annName is "GetMapping" for `@GetMapping("/x")`.
func annName(a string) string {
	a = strings.TrimPrefix(strings.TrimSpace(a), "@")
	if i := strings.IndexAny(a, "( "); i >= 0 {
		a = a[:i]
	}
	return a
}

var (
	annString  = regexp.MustCompile(`"([^"]*)"`)
	annMethod  = regexp.MustCompile(`RequestMethod\.([A-Z]+)`)
	ktorRoute  = regexp.MustCompile(`^\s*(get|post|put|delete|patch|head|options)\s*\(\s*"([^"]*)"\s*\)\s*\{`)
	jvmVerbAnn = map[string]string{
		"GetMapping": "GET", "PostMapping": "POST", "PutMapping": "PUT", "DeleteMapping": "DELETE", "PatchMapping": "PATCH",
		"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE", "Patch": "PATCH", "Head": "HEAD", "Options": "OPTIONS",
		"GET": "GET", "POST": "POST", "PUT": "PUT", "DELETE": "DELETE", "PATCH": "PATCH", "HEAD": "HEAD", "OPTIONS": "OPTIONS",
	}
	jvmGuardAnn = map[string]bool{"PreAuthorize": true, "PostAuthorize": true, "Secured": true, "RolesAllowed": true, "Authenticated": true}
)

// annPath is the first string argument of an annotation ("" without one).
func annPath(a string) string {
	if m := annString.FindStringSubmatch(a); m != nil {
		return m[1]
	}
	return ""
}

// jvmRoutes reads Spring (@GetMapping, @RequestMapping), JAX-RS (@GET with
// @Path) and Micronaut (@Get) routes, prefixed by the type's mapping.
func jvmRoutes(typeAnns, fnAnns []string, handler string, line int) []route {
	prefix := ""
	var guards []string
	for _, a := range typeAnns {
		switch annName(a) {
		case "RequestMapping", "Path", "Controller", "RestController":
			if p := annPath(a); p != "" {
				prefix = p
			}
		}
		if jvmGuardAnn[annName(a)] {
			guards = append(guards, clipGuard(a))
		}
	}
	var methods []string
	path, mapped := "", false
	for _, a := range fnAnns {
		name := annName(a)
		switch {
		case jvmVerbAnn[name] != "":
			methods = append(methods, jvmVerbAnn[name])
			if p := annPath(a); p != "" {
				path = p
			}
			mapped = true
		case name == "RequestMapping":
			for _, m := range annMethod.FindAllStringSubmatch(a, -1) {
				methods = append(methods, m[1])
			}
			path, mapped = annPath(a), true
		case name == "Path":
			path = annPath(a)
		}
		if jvmGuardAnn[name] {
			guards = append(guards, clipGuard(a))
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

func clipGuard(a string) string {
	a = strings.TrimPrefix(strings.TrimSpace(a), "@")
	if len(a) > 60 {
		a = a[:60] + "…"
	}
	return a
}

// ktorRoutes finds Ktor's routing blocks: get("/x") { … }.
func ktorRoutes(raw string, line int) []route {
	m := ktorRoute.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	return []route{{kind: model.RouteHTTP, name: strings.ToUpper(m[1]) + " " + m[2], line: line}}
}

var (
	jvmRuntimeExec  = regexp.MustCompile(`\bRuntime\s*\.\s*getRuntime\s*\(\s*\)\s*\.\s*exec\s*\(`)
	jvmProcessB     = regexp.MustCompile(`\bProcessBuilder\s*\(`)
	jvmShellArgs    = regexp.MustCompile(`"(?:/bin/)?(?:sh|bash|zsh|cmd(?:\.exe)?|powershell)"\s*,\s*"(?:-c|/c|-Command)"`)
	jvmSQLCall      = regexp.MustCompile(`\.\s*(?:executeQuery|executeUpdate|executeLargeUpdate|prepareStatement|prepareCall|addBatch|createQuery|createNativeQuery)\s*\(|(?i:jdbc\w*|\w*template|db|database|conn\w*|stmt|statement|session|em|entityManager|sql)\s*\.\s*(?:execute|query|queryForObject|queryForList|queryForMap|update|batchUpdate)\s*\(`)
	jvmSQLWords     = regexp.MustCompile(`(?i)\b(?:select|insert|update|delete|merge)\b[^"]*\b(?:from|into|set|where|values)\b|\bwhere\b`)
	jvmConcat       = regexp.MustCompile(`"\s*\+\s*[A-Za-z_$]|[A-Za-z_$)]\s*\+\s*"|\$\{|\$[A-Za-z_]|String\.format\s*\(|\.formatted\s*\(`)
	jvmDeserialize  = regexp.MustCompile(`\bnew\s+ObjectInputStream\s*\(|\bObjectInputStream\s*\(|\bXMLDecoder\s*\(|\.enableDefaultTyping\s*\(|activateDefaultTyping\s*\(`)
	jvmWeakHash     = regexp.MustCompile(`MessageDigest\s*\.\s*getInstance\s*\(\s*"(?:MD5|MD2|SHA-?1)"`)
	jvmWeakCipher   = regexp.MustCompile(`Cipher\s*\.\s*getInstance\s*\(\s*"(?:DES|DESede|RC2|RC4|Blowfish|AES)(?:/ECB[^"]*)?"`)
	jvmTrustAll     = regexp.MustCompile(`NoopHostnameVerifier|ALLOW_ALL_HOSTNAME_VERIFIER|setHostnameVerifier\s*\(\s*\(?[^)]*\)?\s*->\s*true|HostnameVerifier\s*\{\s*_,\s*_\s*->\s*true|trustAllCerts|InsecureTrustManagerFactory`)
	jvmRandom       = regexp.MustCompile(`\bnew\s+Random\s*\(|\bRandom\s*\(\s*\)|Math\s*\.\s*random\s*\(`)
	jvmSecretWord   = regexp.MustCompile(`(?i)token|password|passwd|secret|nonce|salt|otp|apikey|api_key`)
	jvmSecretAssign = regexp.MustCompile(`(?:String|val|var|const\s+val)\s+([A-Za-z_]\w*)\s*(?::\s*String\??)?\s*=\s*"([^"]{8,})"`)
)

// jvmRules reports Java and Kotlin findings on one line; IDs match the Go,
// Python and JavaScript detectors (rules/code/*.yaml).
func jvmRules(raw, masked string, line int, owner string) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	switch {
	case jvmShellArgs.MatchString(raw) && (jvmProcessB.MatchString(masked) || jvmRuntimeExec.MatchString(masked)):
		if jvmConcat.MatchString(raw) || strings.Count(raw, ",") > 2 {
			add("exec-shell", "a shell runs a command built at run time")
		} else {
			add("exec-variable", "a command runs through a shell")
		}
	case jvmRuntimeExec.MatchString(masked) && !strings.Contains(raw, `exec("`) && !strings.Contains(raw, `exec(new String[]{"`):
		add("exec-variable", "Runtime.exec with a command that is not a literal")
	}
	if jvmSQLCall.MatchString(masked) && jvmConcat.MatchString(raw) && jvmSQLWords.MatchString(raw) {
		add("sql-concat", "a query built by concatenation or a string template")
	}
	if jvmDeserialize.MatchString(masked) {
		add("deserialize", "Java object deserialisation of untrusted data")
	}
	if jvmWeakHash.MatchString(raw) {
		add("weak-hash", "MessageDigest with MD5 or SHA-1")
	}
	if jvmWeakCipher.MatchString(raw) && !strings.Contains(raw, "/GCM") && !strings.Contains(raw, "/CBC") {
		add("weak-cipher", "a broken cipher or ECB mode")
	}
	if jvmTrustAll.MatchString(raw) {
		add("tls-insecure", "certificate or hostname verification disabled")
	}
	if jvmRandom.MatchString(masked) && jvmSecretWord.MatchString(raw) {
		add("insecure-rand", "java.util.Random for a secret value; use SecureRandom")
	}
	if m := jvmSecretAssign.FindStringSubmatch(raw); m != nil && looksSecret(m[1], m[2]) {
		add("hardcoded-secret", fmt.Sprintf("%s is assigned a %d-character literal", m[1], len(m[2])))
	}
	return out
}
