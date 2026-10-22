// Package lang indexes non-Go source code: Python, JavaScript and
// TypeScript (including <script> blocks of Vue single-file components).
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
	"strings"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
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
	isTest   bool
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
		info, err := d.Info()
		if err != nil || info.Size() > maxFileSize {
			return nil //nolint:nilerr // skip unreadable or huge (generated) files
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(p) //nolint:gosec // inside root, not a symlink
		if err != nil {
			return nil //nolint:nilerr // unreadable file: skip
		}
		ix.parse(filepath.ToSlash(rel), lang, src)
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
	_, err := os.Lstat(filepath.Join(dir, ".git"))
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
}

func (ix *indexer) id(kind model.NodeKind, local string) string {
	return model.NodeID(ix.repo, kind, local)
}

func (ix *indexer) parse(rel, lang string, src []byte) {
	var fr *fileResult
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
	}
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
	lines := strings.Count(string(src), "\n") + 1
	fileID := ix.id(model.KindFile, rel)
	ix.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindFile, Repo: ix.repo, Name: path.Base(rel), File: rel, Package: module,
		IsTest: fr.isTest, LOC: lines, Bytes: len(src), Language: lang,
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
			ix.g.AddEdge(model.Edge{From: fileID, To: sid, Kind: model.EdgeReferences})
		}
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
}

// link resolves imports and calls once every file is parsed, and emits findings.
func (ix *indexer) link() {
	byModule := map[string]*parsedFile{}
	// Name indexes for calls the import graph cannot resolve: methods on
	// values of unknown type (obj.save()) and auto-imported functions (Nuxt
	// composables). Only names that are unique in the repository are used,
	// so ambiguity never produces a wrong edge.
	methodsByName := map[string][]string{}
	funcsByName := map[string][]string{}
	ctorOf := map[string]string{} // class node id -> constructor id
	for _, pf := range ix.files {
		byModule[pf.lang+":"+pf.module] = pf
		for _, s := range pf.fr.symbols {
			sid := pf.symIDs[s.name]
			short := lastPart(s.name)
			switch s.kind {
			case model.KindMethod:
				if !pf.fr.isTest && short != "__init__" && short != "constructor" {
					methodsByName[pf.lang+":"+short] = append(methodsByName[pf.lang+":"+short], sid)
				}
				if short == "__init__" || short == "constructor" {
					ctorOf[pf.symIDs[s.class]] = sid
				}
			case model.KindFunction:
				if s.exported && !pf.fr.isTest {
					family := pf.lang
					if family == Vue || family == TypeScript {
						family = JavaScript
					}
					funcsByName[family+":"+short] = append(funcsByName[family+":"+short], sid)
				}
			}
		}
	}
	unique := func(m map[string][]string, key string) string {
		if ids := m[key]; len(ids) == 1 {
			return ids[0]
		}
		return ""
	}
	for _, pf := range ix.files {
		// local name -> target node id, via imports
		bound := map[string]string{}
		moduleAlias := map[string]*parsedFile{}
		for _, imp := range pf.fr.imports {
			target := ix.resolveImport(pf, imp.spec, byModule)
			if target == nil {
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
				if sid, ok := target.symIDs[member]; ok {
					bound[local] = sid
				}
			}
		}
		family := pf.lang
		if family == Vue || family == TypeScript {
			family = JavaScript
		}
		resolve := func(c call, class string) string {
			switch c.recv {
			case "":
				if sid, ok := pf.symIDs[c.name]; ok {
					return sid
				}
				if sid, ok := bound[c.name]; ok {
					return sid
				}
				if pf.lang != Python {
					return unique(funcsByName, family+":"+c.name)
				}
				return ""
			case "self", "this", "cls":
				if class != "" {
					return pf.symIDs[class+"."+c.name]
				}
			default:
				if m, ok := moduleAlias[c.recv]; ok {
					return m.symIDs[c.name]
				}
				if sid, ok := pf.symIDs[c.recv+"."+c.name]; ok { // Class.static()
					return sid
				}
				return unique(methodsByName, pf.lang+":"+c.name)
			}
			return ""
		}
		link := func(from, to string) {
			if to == "" || to == from {
				return
			}
			ix.g.AddEdge(model.Edge{From: from, To: to, Kind: model.EdgeCalls})
			if ctor, ok := ctorOf[to]; ok { // Class(...) runs its constructor
				ix.g.AddEdge(model.Edge{From: from, To: ctor, Kind: model.EdgeCalls})
			}
		}
		for _, s := range pf.fr.symbols {
			from := pf.symIDs[s.name]
			for _, c := range s.calls {
				link(from, resolve(c, s.class))
			}
		}
		top := pf.id
		if tid, ok := pf.symIDs["<tests>"]; ok {
			top = tid
		}
		for _, c := range pf.fr.topCalls {
			link(top, resolve(c, ""))
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
func (ix *indexer) resolveImport(pf *parsedFile, spec string, byModule map[string]*parsedFile) *parsedFile {
	switch pf.lang {
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
	ext := filepath.Ext(rel)
	return strings.TrimSuffix(rel, ext)
}

func isTestPath(rel, lang string) bool {
	base := path.Base(rel)
	switch lang {
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
