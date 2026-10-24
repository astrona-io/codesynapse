// Package lang indexes non-Go source code: Python, JavaScript and
// TypeScript (including <script> blocks of Vue single-file components),
// Java and Kotlin (brace.go and jvm.go), Rust (rust.go), C# (csharp.go) and PHP (php.go).
//
// The parsers are structural, not full compilers: they find functions,
// classes and methods with exact line ranges, imports between files, calls
// resolved through imports and the enclosing file or class, tests, and
// security findings. Call edges are therefore name-based and can miss
// dynamic dispatch; Go code keeps its type-checked call graph. Everything is
// pure Go, so binaries stay static without cgo.
package lang

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Languages.
const (
	Python     = "python"
	JavaScript = "javascript"
	TypeScript = "typescript"
	Vue        = "vue"
)

const maxFileSize = 1 << 20

var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, ".nuxt": true, ".output": true, ".next": true,
	"dist": true, "build": true, "coverage": true, "__pycache__": true, ".venv": true, "venv": true,
	".tox": true, ".mypy_cache": true, ".pytest_cache": true, "site-packages": true, "testdata": true,
}

// Language returns the language of a file name, or "".
func Language(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".py":
		return Python
	case ".js", ".jsx", ".mjs", ".cjs":
		return JavaScript
	case ".ts", ".tsx", ".mts", ".cts":
		if strings.HasSuffix(name, ".d.ts") {
			return "" // declarations: no code
		}
		return TypeScript
	case ".vue":
		return Vue
	case ".java":
		return Java
	case ".kt", ".kts":
		return Kotlin
	case ".rs":
		return Rust
	case ".cs":
		return CSharp
	case ".php":
		if strings.HasSuffix(strings.ToLower(name), ".blade.php") {
			return "" // a Blade template, not PHP code
		}
		return PHP
	}
	return ""
}

// symbol is a parsed declaration before it becomes a node.
type symbol struct {
	name       string // local name, "Class.method" for methods
	kind       model.NodeKind
	start, end int
	signature  string
	doc        string
	exported   bool
	isTest     bool
	root       bool // decorated / registered: treat as an entry point
	complexity int
	calls      []call
	class      string
	clone      string // structural fingerprint, see cloneHash
	cloneSig   string
}

type call struct {
	recv, name string // recv is "" for plain calls, "self"/"this" for methods
	line       int
}

// fileResult is what a language parser returns for one file.
type fileResult struct {
	symbols  []*symbol
	imports  []importRef
	topCalls []call // module-level calls, attributed to the file
	findings []finding
	routes   []route // entry points registered here (routes.go)
	pkg      string  // declared package (Java, Kotlin), "" otherwise
	isTest   bool
	// partial is set when the source ended with open brackets or an open
	// string: declarations after the problem may be missing or misplaced.
	partial bool
}

// unbalanced reports whether opening and closing brackets in masked source
// fail to pair up: a close without an open, or opens left at the end.
func unbalanced(masked string, opening, closing byte) bool {
	d := 0
	for i := range len(masked) {
		switch masked[i] {
		case opening:
			d++
		case closing:
			if d--; d < 0 {
				return true
			}
		}
	}
	return d != 0
}

// importRef is one import: spec as written ("./api", "pkg.mod", "lodash")
// and the local names it binds (name -> imported member, "*" for modules).
type importRef struct {
	spec  string
	names map[string]string
	line  int
}

type finding struct {
	rule, detail string
	line         int
	owner        string // symbol local name, "" for the file
}

// Result is the output of Extract.
type Result struct {
	Graph *model.Graph
	// ExternalImports maps third-party package names (npm or Python
	// distribution-ish names) to importing file IDs.
	ExternalImports map[string][]string
	// Languages counts indexed files per language.
	Languages map[string]int
}

// Extract indexes every Python, JavaScript, TypeScript and Vue file under root.
func Extract(ctx context.Context, repo, root string) (*Result, error) {
	ix := &indexer{repo: repo, g: model.NewGraph(), files: map[string]*parsedFile{}, res: &Result{ExternalImports: map[string][]string{}, Languages: map[string]int{}}}
	ix.g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if allowed, policyErr := repoconfig.FromContext(ctx).WalkEntry(root, p, d); !allowed || policyErr != nil {
			return policyErr
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || isNestedRepo(p)) {
				return filepath.SkipDir
			}
			return nil
		}
		lang := Language(d.Name())
		if lang == "" || d.Type()&fs.ModeSymlink != 0 || strings.HasSuffix(d.Name(), ".min.js") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			ingestreport.Observe(ctx, rel, ingestreport.Language, ingestreport.StageFailed, "metadata_unavailable")
			return nil //nolint:nilerr // the ledger retains the file failure while extraction continues
		}
		if info.Size() > maxFileSize {
			ingestreport.Observe(ctx, rel, ingestreport.Language, ingestreport.StageFailed, "size_limit")
			return nil
		}
		src, err := os.ReadFile(p) //nolint:gosec // inside root, not a symlink
		if err != nil {
			ingestreport.Observe(ctx, rel, ingestreport.Language, ingestreport.StageFailed, "read_failed")
			return nil //nolint:nilerr // unreadable file: skip
		}
		ix.parse(filepath.ToSlash(rel), lang, src)
		ingestreport.Observe(ctx, rel, ingestreport.Language, ingestreport.StageSucceeded, "")
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking source files: %w", err)
	}
	ix.link()
	ix.res.Graph = ix.g
	return ix.res, nil
}

func isNestedRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git")) //nolint:gosec // a directory of the walk, inside root
	return err == nil
}

type parsedFile struct {
	rel, lang, module string
	id                string
	fr                *fileResult
	symIDs            map[string]string // local name -> node id
}

type indexer struct {
	repo  string
	g     *model.Graph
	files map[string]*parsedFile // rel -> file
	res   *Result
	// rustMods holds the module names of each Rust crate (rust.go).
	rustMods map[string]map[string]bool
}

func (ix *indexer) id(kind model.NodeKind, local string) string {
	return model.NodeID(ix.repo, kind, local)
}

func (ix *indexer) parse(rel, lang string, src []byte) {
	// A byte order mark (common in C# files) would hide the first line's
	// namespace or using from the line-anchored patterns.
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	var fr *fileResult
	var fr0pkg string
	offset := 0
	code := src // the text the parser saw: symbol lines index into it
	switch lang {
	case Python:
		fr = parsePython(src)
	case JavaScript, TypeScript:
		fr = parseJS(src, lang == TypeScript)
	case Vue:
		script, start := vueScript(src)
		if script == nil {
			fr = &fileResult{}
		} else {
			fr = parseJS(script, true)
			offset = start
			code = script
		}
		fr.findings = append(fr.findings, vueRules(src)...)
	case Java:
		fr, fr0pkg = parseBrace(src, javaLang)
	case Kotlin:
		fr, fr0pkg = parseBrace(src, kotlinLang)
	case Rust:
		fr, _ = parseBrace(src, rustLang)
	case CSharp:
		fr, fr0pkg = parseBrace(src, csharpLang)
	case PHP:
		fr, fr0pkg = parseBrace(src, phpLang)
	}
	fr.pkg = fr0pkg
	fr.fingerprint(lang, code)
	if offset > 0 {
		fr.shift(offset)
	}
	if lang == Vue {
		// <script setup> bindings are used by the template, which is not
		// parsed for calls: treat them as entry points of the component.
		for _, s := range fr.symbols {
			s.root = true
		}
	}
	if isTestPath(rel, lang) {
		fr.isTest = true
	}
	ix.res.Languages[lang]++

	module := moduleName(rel, lang)
	if fr.pkg != "" {
		module = fr.pkg // a JVM package or C#/PHP namespace spans files and folders
	}
	lines := strings.Count(string(src), "\n") + 1
	fileID := ix.id(model.KindFile, rel)
	analysis := model.Analysis{Level: model.LevelSyntactic, Status: model.StatusComplete}
	if fr.partial {
		analysis.Status, analysis.Errors = model.StatusPartial, 1
	}
	ix.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindFile, Repo: ix.repo, Name: path.Base(rel), File: rel, Package: module,
		IsTest: fr.isTest, LOC: lines, Bytes: len(src), Language: lang, Analysis: analysis.Encode(),
	})
	pkgID := ix.id(model.KindPackage, module)
	ix.g.AddNode(model.Node{ID: pkgID, Kind: model.KindPackage, Repo: ix.repo, Name: path.Base(module), QualifiedName: module, Package: module, Language: lang})
	ix.g.AddEdge(model.Edge{From: model.NodeID(ix.repo, model.KindRepository, ix.repo), To: pkgID, Kind: model.EdgeContains})
	ix.g.AddEdge(model.Edge{From: pkgID, To: fileID, Kind: model.EdgeContains})

	pf := &parsedFile{rel: rel, lang: lang, module: module, id: fileID, fr: fr, symIDs: map[string]string{}}
	ix.files[rel] = pf
	for _, s := range fr.symbols {
		qn := module + "." + s.name
		sid := ix.id(s.kind, rel+"::"+s.name)
		pf.symIDs[s.name] = sid
		n := model.Node{
			ID: sid, Kind: s.kind, Repo: ix.repo, Name: lastPart(s.name), QualifiedName: qn, Package: module, File: rel,
			StartLine: s.start, EndLine: max(s.start, s.end), LOC: max(s.start, s.end) - s.start + 1, Signature: s.signature,
			Summary: redact.Summary(firstSentence(s.doc), 200), Exported: s.exported, IsTest: s.isTest || (fr.isTest && s.kind != model.KindType && strings.Contains(strings.ToLower(s.name), "test")),
			Complexity: s.complexity, Language: lang,
		}
		if fr.isTest && s.kind != model.KindType {
			n.IsTest = true
		}
		n.SecurityTags = tagsFor(s.calls, lang)
		if !n.IsTest {
			n.CloneHash, n.CloneSig = s.clone, s.cloneSig
		}
		ix.g.AddNode(n)
		ix.g.AddEdge(model.Edge{From: fileID, To: sid, Kind: model.EdgeDefines})
		if s.class != "" {
			if cid, ok := pf.symIDs[s.class]; ok {
				ix.g.AddEdge(model.Edge{From: cid, To: sid, Kind: model.EdgeHasMethod})
			}
		}
		if s.root {
			// Decorated (Flask/FastAPI routes, pytest fixtures, click commands)
			// or registered functions run by a framework: referenced by the file.
			ix.g.AddEdge(model.Edge{From: fileID, To: sid, Kind: model.EdgeReferences, Evidence: model.EvidenceSyntactic})
		}
	}
	if !fr.isTest {
		ix.addRoutes(pf, module)
	}
	// A test file's module-level code (describe/it blocks) is test code:
	// give it a synthetic test function so test reachability works.
	if fr.isTest && len(fr.topCalls) > 0 {
		tid := ix.id(model.KindFunction, rel+"::<tests>")
		ix.g.AddNode(model.Node{ID: tid, Kind: model.KindFunction, Repo: ix.repo, Name: "<tests>", QualifiedName: module + ".<tests>",
			Package: module, File: rel, StartLine: 1, EndLine: lines, IsTest: true, Language: lang})
		pf.symIDs["<tests>"] = tid
	}
}

// fingerprint sets symbol.clone on the functions and methods of a file whose
// lines index into code.
func (fr *fileResult) fingerprint(lang string, code []byte) {
	lines := bytes.Split(code, []byte("\n"))
	for _, s := range fr.symbols {
		if s.kind != model.KindFunction && s.kind != model.KindMethod || s.start < 1 || s.end-s.start+1 < minCloneLOC {
			continue
		}
		end := min(s.end, len(lines))
		if s.start > end {
			continue
		}
		s.clone, s.cloneSig = cloneHash(lang, bytes.Join(lines[s.start-1:end], []byte("\n")))
	}
}

func (fr *fileResult) shift(offset int) {
	for _, s := range fr.symbols {
		s.start += offset
		s.end += offset
		for i := range s.calls {
			s.calls[i].line += offset
		}
	}
	for i := range fr.imports {
		fr.imports[i].line += offset
	}
	for i := range fr.findings {
		fr.findings[i].line += offset
	}
	for i := range fr.routes {
		fr.routes[i].line += offset
	}
}

// link resolves imports and calls once every file is parsed, and emits findings.
func (ix *indexer) link() {
	byModule := map[string]*parsedFile{}
	byPkg := map[string][]*parsedFile{} // JVM package or C# namespace -> its files
	// Name indexes for calls the import graph cannot resolve: methods on
	// values of unknown type (obj.save()) and auto-imported functions (Nuxt
	// composables). Only names that are unique in the repository are used,
	// so ambiguity never produces a wrong edge.
	methodsByName := map[string][]string{}
	funcsByName := map[string][]string{}
	ctorOf := map[string]string{} // class node id -> constructor id
	for _, pf := range ix.files {
		byModule[pf.lang+":"+pf.module] = pf
		if pf.fr.pkg != "" {
			byPkg[pf.fr.pkg] = append(byPkg[pf.fr.pkg], pf)
		}
		for _, s := range pf.fr.symbols {
			sid := pf.symIDs[s.name]
			short := lastPart(s.name)
			switch s.kind {
			case model.KindMethod:
				if !pf.fr.isTest && short != "__init__" && short != "constructor" && short != "__construct" {
					methodsByName[family(pf.lang)+":"+short] = append(methodsByName[family(pf.lang)+":"+short], sid)
				}
				if short == "__init__" || short == "constructor" || short == "__construct" || short == lastPart(s.class) && classBased(pf.lang) {
					ctorOf[pf.symIDs[s.class]] = sid
				}
			case model.KindFunction:
				if s.exported && !pf.fr.isTest {
					funcsByName[family(pf.lang)+":"+short] = append(funcsByName[family(pf.lang)+":"+short], sid)
				}
			}
		}
	}
	for _, files := range byPkg {
		slices.SortFunc(files, func(a, b *parsedFile) int { return strings.Compare(a.rel, b.rel) })
	}
	unique := func(m map[string][]string, key string) (string, model.Evidence) {
		if ids := m[key]; len(ids) == 1 {
			return ids[0], model.EvidenceInferred
		}
		return "", ""
	}
	for _, pf := range ix.files {
		// local name -> target node id, via imports
		bound := map[string]string{}
		boundFile := map[string]*parsedFile{} // imported type -> its file
		boundName := map[string]string{}      // local name -> declared name (import aliases)
		moduleAlias := map[string]*parsedFile{}
		// Namespaces whose types need no qualification: the file's own
		// package, and for C# its parents and every using directive.
		scope := []string{pf.fr.pkg}
		if pf.lang == CSharp {
			for ns := pf.fr.pkg; strings.Contains(ns, "."); {
				ns = ns[:strings.LastIndexByte(ns, '.')]
				scope = append(scope, ns)
			}
		}
		for _, imp := range pf.fr.imports {
			if pf.lang == CSharp && len(imp.names) == 0 {
				if csLocalNamespace(byPkg, imp.spec) {
					scope = append(scope, imp.spec)
				} else if ext := externalName(imp.spec, pf.lang); ext != "" {
					ix.res.ExternalImports[ext] = append(ix.res.ExternalImports[ext], pf.id)
				}
				continue
			}
			target := ix.resolveImport(pf, imp, byModule, byPkg)
			if target == nil {
				if pf.lang == Rust && ix.rustLocal(pf, imp.spec) || pf.lang == CSharp && csLocalNamespace(byPkg, imp.spec) {
					continue
				}
				if ext := externalName(imp.spec, pf.lang); ext != "" {
					ix.res.ExternalImports[ext] = append(ix.res.ExternalImports[ext], pf.id)
				}
				continue
			}
			ix.g.AddEdge(model.Edge{From: ix.id(model.KindPackage, pf.module), To: ix.id(model.KindPackage, target.module), Kind: model.EdgeImports})
			for local, member := range imp.names {
				if member == "*" {
					moduleAlias[local] = target
					continue
				}
				// use crate::db; binds a module: db::open() calls into it.
				if pf.lang == Rust {
					if m := ix.rustResolve(pf, imp.spec+"::"+member, true); m != nil {
						moduleAlias[local] = m
						continue
					}
				}
				if sid, ok := target.symIDs[member]; ok {
					bound[local] = sid
					boundFile[local] = target
					boundName[local] = member
				}
			}
		}
		fam := family(pf.lang)
		// samePackage finds a declaration in another file of a JVM package
		// or a C# namespace in scope, which needs no import.
		samePackage := func(name string) string {
			for _, ns := range scope {
				for _, f := range byPkg[ns] {
					if sid, ok := f.symIDs[name]; ok && ns != "" {
						return sid
					}
				}
			}
			return ""
		}
		// resolve finds a call's target and says how: through declarations
		// and imports (syntactic) or a repository-unique name (inferred).
		resolve := func(c call, class string) (string, model.Evidence) {
			if pf.lang == PHP && (c.recv == "$this" || c.recv == "static") {
				c.recv = "this"
			}
			declared := func(sid string) (string, model.Evidence) {
				if sid == "" {
					return "", ""
				}
				return sid, model.EvidenceSyntactic
			}
			switch c.recv {
			case "":
				if sid, ok := pf.symIDs[c.name]; ok {
					return declared(sid)
				}
				if sid, ok := bound[c.name]; ok {
					return declared(sid)
				}
				if classBased(pf.lang) {
					// Inside a class a plain call is one of its methods.
					if sid, ok := pf.symIDs[class+"."+c.name]; ok && class != "" {
						return declared(sid)
					}
					if sid := samePackage(c.name); sid != "" {
						return declared(sid)
					}
					// Java and C# have no free functions: a call without a receiver
					// name is a method of a chained expression (new A().f()).
					if sid, ev := unique(funcsByName, fam+":"+c.name); sid != "" {
						return sid, ev
					}
					return unique(methodsByName, fam+":"+c.name)
				}
				if pf.lang == PHP {
					if sid := samePackage(c.name); sid != "" { // a function of the namespace
						return declared(sid)
					}
				}
				if pf.lang != Python {
					return unique(funcsByName, fam+":"+c.name)
				}
			case "self", "this", "cls":
				if class != "" {
					return declared(pf.symIDs[class+"."+c.name])
				}
			default:
				if m, ok := moduleAlias[c.recv]; ok {
					return declared(m.symIDs[c.name])
				}
				if sid, ok := pf.symIDs[c.recv+"."+c.name]; ok { // Class.static()
					return declared(sid)
				}
				if f, ok := boundFile[c.recv]; ok { // ImportedClass.static(), also through an alias
					return declared(f.symIDs[boundName[c.recv]+"."+c.name])
				}
				if sid := samePackage(c.recv + "." + c.name); sid != "" {
					return declared(sid)
				}
				return unique(methodsByName, fam+":"+c.name)
			}
			return "", ""
		}
		link := func(from string, c call, class string) {
			to, evidence := resolve(c, class)
			if to == "" || to == from {
				return
			}
			ix.g.AddEdge(model.Edge{From: from, To: to, Kind: model.EdgeCalls, Evidence: evidence})
			if ctor, ok := ctorOf[to]; ok { // Class(...) runs its constructor
				ix.g.AddEdge(model.Edge{From: from, To: ctor, Kind: model.EdgeCalls, Evidence: evidence})
			}
		}
		for _, s := range pf.fr.symbols {
			from := pf.symIDs[s.name]
			for _, c := range s.calls {
				link(from, c, s.class)
			}
		}
		top := pf.id
		if tid, ok := pf.symIDs["<tests>"]; ok {
			top = tid
		}
		for _, c := range pf.fr.topCalls {
			link(top, c, "")
		}
		for _, f := range pf.fr.findings {
			if pf.fr.isTest {
				continue // fixtures with fake secrets would bury real findings
			}
			owner := pf.id
			if f.owner != "" {
				if sid, ok := pf.symIDs[f.owner]; ok {
					owner = sid
				}
			}
			ix.report(pf, owner, f)
		}
	}
}

// resolveImport maps an import spec to an in-repo file.
func (ix *indexer) resolveImport(pf *parsedFile, imp importRef, byModule map[string]*parsedFile, byPkg map[string][]*parsedFile) *parsedFile {
	spec := imp.spec
	switch pf.lang {
	case Rust:
		return ix.rustResolve(pf, spec, false)
	case Java, Kotlin, CSharp, PHP:
		// com.acme.Foo is Foo in package com.acme, in Java or Kotlin; a C#
		// alias or static using names a type the same way.
		for _, member := range imp.names {
			first, _, _ := strings.Cut(member, ".")
			pkg := spec
			if !strings.Contains(member, ".") {
				pkg = strings.TrimSuffix(spec, "."+member)
			}
			for _, f := range byPkg[pkg] {
				if _, ok := f.symIDs[first]; ok {
					return f
				}
			}
		}
		return nil
	case Python:
		mod := spec
		if strings.HasPrefix(spec, ".") { // relative import
			dots := len(spec) - len(strings.TrimLeft(spec, "."))
			base := strings.Split(pf.module, ".")
			if len(base) >= dots {
				base = base[:len(base)-dots]
			}
			rest := strings.TrimLeft(spec, ".")
			mod = strings.Join(append(base, strings.Split(rest, ".")...), ".")
			mod = strings.Trim(mod, ".")
		}
		for _, candidate := range []string{mod, strings.TrimSuffix(mod, ".__init__")} {
			if f, ok := byModule[Python+":"+candidate]; ok {
				return f
			}
			// src layout: "pkg.mod" stored as "src.pkg.mod"
			for key, f := range byModule {
				if strings.HasPrefix(key, Python+":") && strings.HasSuffix(key, "."+candidate) {
					return f
				}
			}
		}
	default:
		if !strings.HasPrefix(spec, ".") && !strings.HasPrefix(spec, "@/") && !strings.HasPrefix(spec, "~/") {
			return nil
		}
		var base string
		switch {
		case strings.HasPrefix(spec, "@/"), strings.HasPrefix(spec, "~/"):
			base = path.Join(srcRoot(pf.rel), spec[2:])
		default:
			base = path.Join(path.Dir(pf.rel), spec)
		}
		for _, suffix := range []string{"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".vue", "/index.ts", "/index.js", "/index.tsx"} {
			if f, ok := ix.files[strings.TrimSuffix(base, ".js")+suffix]; ok {
				return f
			}
		}
	}
	return nil
}

// srcRoot guesses the directory "@/" aliases point at (src/ or app/).
func srcRoot(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		if p == "src" || p == "app" {
			return strings.Join(parts[:i+1], "/")
		}
	}
	return "."
}

// externalName returns the package name of a third-party import.
func externalName(spec, lang string) string {
	if lang == Rust {
		first, _, _ := strings.Cut(spec, "::")
		switch first {
		case "crate", "self", "super", "std", "core", "alloc", "proc_macro", "test", "":
			return ""
		}
		return first // a crate: Cargo advisories become "imported"
	}
	if isJVM(lang) || lang == PHP {
		return "" // Maven and Composer names do not follow package names
	}
	if lang == CSharp {
		return spec // a namespace; NuGet IDs usually prefix it (internal/vuln)
	}
	if lang == Python {
		if strings.HasPrefix(spec, ".") {
			return ""
		}
		top, _, _ := strings.Cut(spec, ".")
		return top
	}
	if strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "@/") || strings.HasPrefix(spec, "~/") || strings.HasPrefix(spec, "node:") {
		return ""
	}
	parts := strings.Split(spec, "/")
	if strings.HasPrefix(spec, "@") && len(parts) > 1 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func moduleName(rel, lang string) string {
	if lang == Python {
		m := strings.TrimSuffix(rel, ".py")
		m = strings.TrimSuffix(m, "/__init__")
		return strings.ReplaceAll(m, "/", ".")
	}
	if lang == Rust {
		root, mod := rustModule(rel)
		crate := path.Base(root)
		if root == "." || root == "" {
			crate = "crate"
		}
		return strings.Trim(crate+"::"+mod, ":")
	}
	ext := filepath.Ext(rel)
	return strings.TrimSuffix(rel, ext)
}

func isTestPath(rel, lang string) bool {
	base := path.Base(rel)
	switch lang {
	case Rust:
		return strings.Contains("/"+rel, "/tests/") || strings.Contains("/"+rel, "/benches/")
	case PHP:
		return strings.Contains("/"+strings.ToLower(rel), "/tests/") || strings.HasSuffix(base, "Test.php")
	case CSharp:
		stem := strings.TrimSuffix(base, ".cs")
		for _, dir := range strings.Split(path.Dir(rel), "/") {
			if strings.HasSuffix(dir, ".Tests") || strings.HasSuffix(dir, ".Test") || strings.HasSuffix(dir, ".UnitTests") ||
				strings.HasSuffix(dir, ".IntegrationTests") || dir == "test" || dir == "tests" {
				return true
			}
		}
		return strings.HasSuffix(stem, "Tests") || strings.HasSuffix(stem, "Test")
	case Java, Kotlin:
		stem := strings.TrimSuffix(strings.TrimSuffix(base, path.Ext(base)), ".")
		return strings.Contains("/"+rel, "/src/test/") || strings.HasSuffix(stem, "Test") || strings.HasSuffix(stem, "Tests") ||
			strings.HasSuffix(stem, "IT") || strings.HasSuffix(stem, "Spec")
	case Python:
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") || base == "conftest.py" || strings.Contains("/"+rel, "/tests/")
	default:
		return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.Contains("/"+rel, "/__tests__/") || strings.Contains("/"+rel, "/e2e/")
	}
}

func lastPart(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	return s
}

// countBranches is a rough cyclomatic complexity for a block of source.
func countBranches(body string, keywords []string) int {
	c := 1
	for _, kw := range keywords {
		c += countWord(body, kw)
	}
	c += strings.Count(body, "&&") + strings.Count(body, "||")
	return c
}

func countWord(s, w string) int {
	n := 0
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return n
		}
		j += i
		before := j == 0 || !isIdent(rune(s[j-1]))
		after := j+len(w) >= len(s) || !isIdent(rune(s[j+len(w)]))
		if before && after {
			n++
		}
		i = j + len(w)
	}
}

func isIdent(r rune) bool {
	return r == '_' || r == '$' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// addRoutes adds the file's entry points as Route nodes, each linked to its
// handler when that is a function of the file.
func (ix *indexer) addRoutes(pf *parsedFile, module string) {
	for _, r := range pf.fr.routes {
		local := r.kind + " " + r.name + " (" + module + ")"
		id := ix.id(model.KindRoute, local)
		ix.g.AddNode(model.Node{
			ID: id, Kind: model.KindRoute, Repo: ix.repo, Name: r.name, QualifiedName: local, Package: module, File: pf.rel,
			StartLine: r.line, EndLine: r.line, Signature: r.kind, Language: pf.lang,
			Summary: redact.Summary(strings.Join(r.guards[:min(len(r.guards), 4)], "; "), 200),
		})
		if sid, ok := pf.symIDs[r.handler]; ok && r.handler != "" {
			ix.g.AddEdge(model.Edge{From: id, To: sid, Kind: model.EdgeHandledBy, Evidence: model.EvidenceSyntactic})
		}
	}
}

// family groups languages whose code calls each other by name.
func family(lang string) string {
	switch lang {
	case Vue, TypeScript:
		return JavaScript
	case Kotlin:
		return Java
	}
	return lang
}

func isJVM(lang string) bool { return lang == Java || lang == Kotlin }

// classBased languages have methods in classes and constructors named
// after them, and resolve plain calls within the class and its package.
func classBased(lang string) bool { return isJVM(lang) || lang == CSharp }
