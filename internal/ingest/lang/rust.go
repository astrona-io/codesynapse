package lang

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Rust on the brace engine: items (struct, enum, trait, fn), impl blocks
// as scopes for methods, use paths resolved to files of the crate, #[test]
// functions, actix/rocket/axum routes, and Rust rules.

// Rust is the language name of .rs files.
const Rust = "rust"

const rustVis = `(?:pub(?:\([^)]*\))?\s+)?`

var (
	rustType     = regexp.MustCompile(`^\s*` + rustVis + `(?:unsafe\s+)?(?:struct|enum|trait|union|type)\s+([A-Za-z_]\w*)`)
	rustFunc     = regexp.MustCompile(`^\s*` + rustVis + `(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+(?P<name>[A-Za-z_]\w*)`)
	rustAttr     = regexp.MustCompile(`^\s*#!?\[`)
	rustUse      = regexp.MustCompile(`^\s*` + rustVis + `use\s+(.+?);`)
	rustModDecl  = regexp.MustCompile(`^\s*` + rustVis + `mod\s+([A-Za-z_]\w*)\s*;`)
	rustKeywords = setOf("if while for loop match return Some Ok Err None Box Vec String self Self super crate as in let mut ref move fn impl where async await unsafe dyn struct enum trait type use mod pub")
	rustBranches = []string{"if", "for", "while", "loop", "match", "=>"}
)

var rustLang = &braceLang{
	lang: Rust, typeDecl: rustType, funcDecl: rustFunc, annotation: rustAttr, topFuncs: true, rust: true,
	scope: rustImplOrMod,
	imports: func(raw string, line int) (importRef, bool) {
		if m := rustModDecl.FindStringSubmatch(raw); m != nil {
			// mod db; declares a child module file: db::open() calls into it.
			return importRef{spec: "self::" + m[1], names: map[string]string{m[1]: "*"}, line: line}, true
		}
		m := rustUse.FindStringSubmatch(raw)
		if m == nil {
			return importRef{}, false
		}
		return rustUseRef(m[1], line), true
	},
	keywords: rustKeywords, branches: rustBranches,
	exported:      func(decl string) bool { return strings.Contains(decl, "pub ") || strings.Contains(decl, "pub(") },
	testAnnotated: rustTestAttr, rules: rustRules, routes: rustAttrRoutes, lineRoutes: axumRoutes,
}

// rustImplOrMod is the scope of impl blocks (impl Type, impl Trait for
// Type) and of inline modules (mod tests {).
var rustImplOrMod = regexp.MustCompile(`^\s*(?:unsafe\s+)?impl\b(?:\s*<[^{]*?>)?\s+(?:[\w:]+(?:<[^{]*?>)?\s+for\s+)?&?(?:mut\s+)?(?:\w+::)*([A-Za-z_]\w*)|^\s*` + rustVis + `mod\s+([A-Za-z_]\w*)\s*\{`)

// rustUseRef reads "crate::a::b::{C, d as e}" into an import of module
// crate::a::b with C and e bound; a single path binds its last segment.
func rustUseRef(spec string, line int) importRef {
	spec = strings.TrimSpace(spec)
	ref := importRef{names: map[string]string{}, line: line}
	if open := strings.Index(spec, "{"); open >= 0 {
		ref.spec = strings.TrimSuffix(strings.TrimSpace(spec[:open]), "::")
		for _, item := range strings.Split(strings.TrimSuffix(spec[open+1:], "}"), ",") {
			item = strings.TrimSpace(item)
			if item == "self" { // {self, User} also binds the module
				ref.names[ref.spec[strings.LastIndex(ref.spec, ":")+1:]] = "*"
				continue
			}
			if item == "" || strings.ContainsAny(item, "{}*") {
				continue
			}
			name, alias, ok := strings.Cut(item, " as ")
			name = strings.TrimSpace(name)
			if strings.Contains(name, "::") { // nested paths: keep the leaf
				name = name[strings.LastIndex(name, "::")+2:]
			}
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
	i := strings.LastIndex(name, "::")
	if i < 0 {
		ref.spec = name
		return ref
	}
	ref.spec, name = name[:i], name[i+2:]
	local := name
	if ok {
		local = strings.TrimSpace(alias)
	}
	if name != "*" && name != "self" {
		ref.names[local] = name
	}
	return ref
}

func rustTestAttr(anns []string) bool {
	for _, a := range anns {
		t := strings.TrimSpace(a)
		if t == "#[test]" || strings.HasPrefix(t, "#[tokio::test") || strings.HasPrefix(t, "#[rstest") || strings.HasPrefix(t, "#[async_std::test") || strings.HasPrefix(t, "#[test_case") {
			return true
		}
	}
	return false
}

// rustModule is the module path of a file within its crate: src/a/b.rs and
// src/a/b/mod.rs are a::b; lib.rs and main.rs are the crate root.
func rustModule(rel string) (root, module string) {
	i := strings.LastIndex("/"+rel, "/src/")
	if i < 0 {
		return path.Dir(rel), strings.TrimSuffix(path.Base(rel), ".rs")
	}
	root = strings.TrimSuffix(rel[:i], "/")
	m := strings.TrimSuffix(rel[i+4:], ".rs")
	m = strings.TrimSuffix(strings.TrimSuffix(m, "/mod"), "mod")
	if m == "lib" || m == "main" || m == "" {
		return root, ""
	}
	return root, strings.ReplaceAll(strings.Trim(m, "/"), "/", "::")
}

// rustResolve finds the crate file a use path names: crate::, self:: and
// super:: paths, tried as a module and as an item of its parent module.
// exact accepts only the whole path as a module, not an item of a parent.
func (ix *indexer) rustResolve(pf *parsedFile, spec string, exact bool) *parsedFile {
	root, here := rustModule(pf.rel)
	segs := strings.Split(spec, "::")
	switch segs[0] {
	case "crate":
		segs = segs[1:]
	case "self":
		segs = append(splitRustPath(here), segs[1:]...)
	case "super":
		parent := splitRustPath(here)
		for len(segs) > 0 && segs[0] == "super" && len(parent) > 0 {
			parent, segs = parent[:len(parent)-1], segs[1:]
		}
		segs = append(parent, segs...)
	default:
		// Since the 2018 edition a path may start at a module in scope:
		// mod db; use db::open;
		if !ix.rustLocal(pf, spec) {
			return nil // another crate, or std
		}
		low := len(splitRustPath(here)) + 1 // never the file itself
		segs = append(splitRustPath(here), segs...)
		if exact {
			low = len(segs)
		}
		return ix.rustFile(root, segs, low)
	}
	low := 0
	if exact {
		low = len(segs)
	}
	return ix.rustFile(root, segs, low)
}

// rustFile is the file of the longest module prefix of segs (at least low
// segments) in the crate at root.
func (ix *indexer) rustFile(root string, segs []string, low int) *parsedFile {
	for k := len(segs); k >= low; k-- {
		mod := strings.Join(segs[:k], "/")
		cands := []string{path.Join(root, "src", mod+".rs"), path.Join(root, "src", mod, "mod.rs")}
		if mod == "" {
			cands = []string{path.Join(root, "src", "lib.rs"), path.Join(root, "src", "main.rs")}
		}
		for _, c := range cands {
			if f, ok := ix.files[c]; ok {
				return f
			}
		}
	}
	return nil
}

// rustLocal reports whether a use path starts with a module of pf's own
// crate rather than another crate's name: a file a.rs or a/mod.rs anywhere
// under the crate's src/ (reached through mod declarations or globs).
func (ix *indexer) rustLocal(pf *parsedFile, spec string) bool {
	first, _, _ := strings.Cut(spec, "::")
	root, _ := rustModule(pf.rel)
	if ix.rustMods == nil {
		ix.rustMods = map[string]map[string]bool{}
		for rel, f := range ix.files {
			if f.lang != Rust {
				continue
			}
			r, m := rustModule(rel)
			if ix.rustMods[r] == nil {
				ix.rustMods[r] = map[string]bool{}
			}
			for _, seg := range splitRustPath(m) {
				ix.rustMods[r][seg] = true
			}
		}
	}
	return ix.rustMods[root][first]
}

func splitRustPath(m string) []string {
	if m == "" {
		return nil
	}
	return strings.Split(m, "::")
}

var (
	rustAttrRoute = regexp.MustCompile(`^\s*#\[\s*(?:actix_web::)?(get|post|put|delete|patch|head|options)\s*\(\s*"([^"]*)"`)
	axumRoute     = regexp.MustCompile(`\.route\(\s*"([^"]*)"\s*,(.*)`)
	axumMethod    = regexp.MustCompile(`\b(get|post|put|delete|patch|head|options)\s*\(\s*([A-Za-z_][\w:]*)\s*\)`)
)

// rustAttrRoutes reads actix-web and rocket attributes: #[get("/x")].
func rustAttrRoutes(_, fnAnns []string, handler string, line int) []route {
	var out []route
	for _, a := range fnAnns {
		if m := rustAttrRoute.FindStringSubmatch(a); m != nil {
			out = append(out, route{kind: model.RouteHTTP, name: strings.ToUpper(m[1]) + " " + m[2], line: line, handler: handler})
		}
	}
	return out
}

// axumRoutes reads .route("/x", get(handler).post(other)).
func axumRoutes(raw string, line int) []route {
	m := axumRoute.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	var out []route
	for _, h := range axumMethod.FindAllStringSubmatch(m[2], -1) {
		handler := h[2]
		if i := strings.LastIndex(handler, "::"); i >= 0 {
			handler = handler[i+2:]
		}
		out = append(out, route{kind: model.RouteHTTP, name: strings.ToUpper(h[1]) + " " + m[1], line: line, handler: handler})
	}
	return out
}

var (
	rustCommand  = regexp.MustCompile(`\bCommand::new\s*\(`)
	rustShell    = regexp.MustCompile(`Command::new\s*\(\s*"(?:/bin/)?(?:sh|bash|zsh|cmd|powershell)"`)
	rustLiteralC = regexp.MustCompile(`Command::new\s*\(\s*"`)
	rustSQL      = regexp.MustCompile(`\b(?:query|query_as|query_scalar|execute|sql_query|prepare|batch_execute)\s*(?:::<[^>]*>)?\s*\(\s*&?\s*format!\s*\(`)
	rustTLS      = regexp.MustCompile(`danger_accept_invalid_certs\s*\(\s*true|danger_accept_invalid_hostnames\s*\(\s*true|SslVerifyMode::NONE|dangerous\(\)\s*\.\s*set_certificate_verifier`)
	rustWeakHash = regexp.MustCompile(`\bmd5::(?:compute|Md5)|\bMd5::new|\bSha1::new|\bsha1::Sha1|\bmd5::Context`)
	rustSecret   = regexp.MustCompile(`\b(?:const|static|let)\s+(?:mut\s+)?([A-Za-z_]\w*)\s*(?::\s*&(?:'static\s+)?str)?\s*=\s*"([^"]{8,})"`)
)

// rustRules reports Rust findings on one line (rules/code/*.yaml).
func rustRules(raw, masked string, line int, owner string) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	switch {
	case rustShell.MatchString(raw) && strings.Contains(raw, `"-c"`):
		if strings.Contains(raw, "format!") || strings.Count(raw, ".arg") > 1 && !strings.Contains(strings.SplitN(raw, `"-c"`, 2)[1], `.arg("`) {
			add("exec-shell", "a shell runs a command built at run time")
		} else {
			add("exec-variable", "a command runs through a shell")
		}
	case rustCommand.MatchString(masked) && !rustLiteralC.MatchString(raw):
		add("exec-variable", "Command::new with a program that is not a literal")
	}
	if rustSQL.MatchString(masked) {
		add("sql-concat", "a query built with format!")
	}
	if rustTLS.MatchString(raw) {
		add("tls-insecure", "certificate or hostname verification disabled")
	}
	if rustWeakHash.MatchString(raw) {
		add("weak-hash", "MD5 or SHA-1")
	}
	if m := rustSecret.FindStringSubmatch(raw); m != nil && looksSecret(m[1], m[2]) {
		add("hardcoded-secret", fmt.Sprintf("%s is assigned a %d-character literal", m[1], len(m[2])))
	}
	return out
}

// rustTags maps Rust library calls to security tags.
func rustTags(recv, n string, set map[string]bool) {
	switch {
	case recv == "Command" && n == "new":
		set["exec"] = true
	case recv == "sqlx" || n == "sql_query" || n == "query_as" || recv == "diesel":
		set["sql"] = true
	case recv == "reqwest" || recv == "hyper" || recv == "ureq" || recv == "Client" && n == "new":
		set["http"] = true
	case recv == "fs" || recv == "File" || recv == "OpenOptions":
		set["filesystem"] = true
	case recv == "env" && (n == "var" || n == "var_os" || n == "vars"):
		set["env"] = true
	case recv == "Sha256" || recv == "Hmac" || recv == "Aes256Gcm" || recv == "ring" || recv == "argon2" || recv == "bcrypt":
		set["crypto"] = true
	case recv == "bincode" || recv == "serde_pickle":
		set["deserialize"] = true
	}
}
