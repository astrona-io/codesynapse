package lang

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Ruby: blocks close with end, not braces, so it has its own parser. It
// masks strings, comments, heredocs and literals, then pairs every end with
// the keyword that opened it (class, module, def, do, and if/unless/while/
// until/case/begin/for at the start of a statement; their modifier forms
// open nothing). Classes and modules are types, def in them methods, and
// constants resolve across files the way Rails autoloads them.

// Ruby is the language name of .rb files.
const Ruby = "ruby"

var (
	// A word before def is a visibility or a decorator (private_class_method def, memoize def).
	rbDef    = regexp.MustCompile(`^\s*(?:([a-z_]\w*)\s+)?def\s+((?:self|@{0,2}[A-Za-z_]\w*)\.)?([A-Za-z_]\w*[?!=]?|\[\]=?|[-+*/%<>=!~^&|]+)\s*(\([^)]*\))?\s*(=[^=~>])?`)
	rbType   = regexp.MustCompile(`^\s*(class|module)\s+(?:::)?([A-Z]\w*(?:::[A-Z]\w*)*)(\s*<\s*([A-Z][\w:]*))?`)
	rbSClass = regexp.MustCompile(`^\s*class\s*<<\s*\S+`) // class << self, class << Rails.application
	// The keyword must not be a hash key (if: cond), hence the [^:\w?!].
	rbOpenStart = regexp.MustCompile(`^\s*(if|unless|while|until|case|begin|for)(?:[^:\w?!]|$)`)
	rbOpenMid   = regexp.MustCompile(`(?:^|[^\w.:@$])(if|unless|case|begin|while|until)(?:[^:\w?!]|$)`)
	rbDefMid    = regexp.MustCompile(`(?:^|[^\w.:@])def\s+(?:self\.)?[^\s(;=]+\s*(?:\([^)]*\))?\s*(=[^=~>])?`) // module X; def one; 1; end; end, { def x; end }
	rbWord      = regexp.MustCompile(`[A-Za-z_]\w*[?!]?`)
	rbRequire   = regexp.MustCompile(`^\s*(require|require_relative|load)\s*\(?\s*['"]([^'"]+)['"]`)
	rbDotCall   = regexp.MustCompile(`(@{0,2}[A-Za-z_]\w*[?!]?|[A-Z]\w*(?:::[A-Z]\w*)+|[)\]])\s*&?(?:\.|::)\s*([a-z_]\w*[?!]?)`)
	rbBare      = regexp.MustCompile(`(?:^|[^\w.:@$?!])([a-z_]\w*[?!]?)`)
	rbSymbolRef = regexp.MustCompile(`(?:^|[\s,(\[])(?::|\bif:\s*:|\bunless:\s*:)([a-z_]\w*[?!]?)`)
	rbKeywords  = setOf("if unless else elsif end def class module do while until for in case when then begin rescue ensure return yield self nil true false and or not super next break redo retry alias defined puts print p pp require require_relative load attr_accessor attr_reader attr_writer include extend prepend private protected public module_function raise lambda proc loop new __method__ block_given binding")
	rbBranches  = []string{"if", "unless", "elsif", "while", "until", "when", "rescue", "&&", "||", "?"}
)

// rbKeywordValue are words after which a space and % or / is an operator
// on a value (x = a % b) rather than a literal argument.
var rbKeywordValue = map[string]bool{"self": true, "nil": true, "true": true, "false": true, "end": true}

// rbClass is what a Ruby class says about its actions: its superclass and
// the before_action callbacks it adds or skips.
type rbClass struct {
	super        string
	before, skip []rbCallback
}

type rbCallback struct {
	name         string
	only, except []string
}

func (c rbCallback) applies(action string) bool {
	switch {
	case len(c.only) > 0:
		return slices.Contains(c.only, action)
	case len(c.except) > 0:
		return !slices.Contains(c.except, action)
	}
	return true
}

var (
	rbBeforeAction = regexp.MustCompile(`^\s*(prepend_before_action|before_action|before_filter|skip_before_action|skip_before_filter)\s*\(?\s*(.*)$`)
	rbCallbackOpt  = regexp.MustCompile(`\b(only|except):\s*(?:\[([^\]]*)\]|%i\[([^\]]*)\]|:(\w+[?!]?))`)
	rbSymbolName   = regexp.MustCompile(`:(\w+[?!]?)`)
)

// rbCallbacks reads "before_action :a, :b, only: [:show]" into callbacks.
func rbCallbacks(args string) []rbCallback {
	names := args
	if j := rbCallbackOpt.FindStringIndex(args); j != nil {
		names = args[:j[0]]
	}
	var only, except []string
	for _, m := range rbCallbackOpt.FindAllStringSubmatch(args, -1) {
		list := m[2] + " " + m[3] + " :" + m[4]
		acts := strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' || r == ':' })
		if m[1] == "only" {
			only = append(only, acts...)
		} else {
			except = append(except, acts...)
		}
	}
	var out []rbCallback
	for _, m := range rbSymbolName.FindAllStringSubmatch(names, -1) {
		out = append(out, rbCallback{name: m[1], only: only, except: except})
	}
	return out
}

// rubyActionGuards are the before_action callbacks that run before action
// of class typ, through its superclasses (ApplicationController), less the
// ones a class skips.
func rubyActionGuards(classes map[string]*rbClass, typ, action string, lookup func(string) (*parsedFile, string)) []string {
	var levels [][]rbCallback
	skipped := map[string]bool{}
	for range 8 {
		c := classes[typ]
		if c == nil {
			break
		}
		levels = append(levels, c.before)
		for _, s := range c.skip {
			if s.applies(action) {
				skipped[s.name] = true
			}
		}
		f, t := lookup(c.super)
		if f == nil {
			break
		}
		classes, typ = f.fr.rbClasses, t
	}
	var out []string
	for i := len(levels) - 1; i >= 0; i-- { // the base class's callbacks run first
		for _, cb := range levels[i] {
			if cb.applies(action) && !skipped[cb.name] {
				out = append(out, "before_action :"+cb.name)
			}
		}
	}
	return out
}

type rbFrame struct {
	kind    string // "type", "def", "block"
	sym     *symbol
	name    string // type name ("A.B") for types
	private bool   // after a bare private in a type body
	single  bool   // class << self
	prefix  string // a Rails routes namespace or scope
	module  string // a Rails routes namespace's controller module (Admin)
}

// parseRuby parses one Ruby file; rel decides whether it is the Rails
// routes file.
func parseRuby(src []byte, rel string) *fileResult {
	raw := strings.Split(string(src), "\n")
	masked := maskRuby(string(src))
	lines := strings.Split(masked, "\n")
	fr := &fileResult{}
	routesFile := strings.HasSuffix(rel, "config/routes.rb") || strings.Contains(rel, "config/routes/")

	var stack []*rbFrame
	typeName := func() (string, *rbFrame) {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == "type" {
				return stack[i].name, stack[i]
			}
		}
		return "", nil
	}
	innerDef := func() *symbol {
		for i := len(stack) - 1; i >= 0; i-- {
			if stack[i].kind == "def" {
				return stack[i].sym
			}
		}
		return nil
	}
	prefix := func() (path, module string) {
		var b strings.Builder
		var mods []string
		for _, f := range stack {
			b.WriteString(f.prefix)
			if f.module != "" {
				mods = append(mods, f.module)
			}
		}
		return b.String(), strings.Join(mods, "::")
	}
	type token struct {
		pos  int
		open *rbFrame // nil: an end
	}
	refs := map[string]bool{} // Class.method named as a :symbol in a class body

	for i, line := range lines {
		n := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		routePrefix, routeModule := prefix() // before this line's own block opens
		var toks []token
		consumed := -1 // the opener keyword already handled on this line
		cls, tf := typeName()
		if m := rbRequire.FindStringSubmatch(raw[i]); m != nil && innerDef() == nil {
			spec := m[2]
			if m[1] == "require_relative" && !strings.HasPrefix(spec, ".") {
				spec = "./" + spec
			}
			fr.imports = append(fr.imports, importRef{spec: spec, names: map[string]string{}, line: n})
		}
		switch {
		case rbSClass.MatchString(line):
			loc := rbSClass.FindStringIndex(line)
			toks = append(toks, token{pos: loc[0], open: &rbFrame{kind: "type", name: cls, single: true}})
			consumed = loc[1]
		case rbType.MatchString(line):
			m := rbType.FindStringSubmatchIndex(line)
			name := strings.ReplaceAll(line[m[4]:m[5]], "::", ".")
			if cls != "" {
				name = cls + "." + name
			}
			s := &symbol{name: name, kind: model.KindType, start: n, end: n, signature: strings.TrimSpace(raw[i]), exported: true, doc: rubyDoc(raw, i)}
			if m[8] >= 0 && rbControllerBase(line[m[8]:m[9]]) || strings.HasSuffix(name, "Controller") {
				s.root = true // marks a controller: its public actions are entry points
			}
			fr.symbols = append(fr.symbols, s)
			if fr.rbClasses == nil {
				fr.rbClasses = map[string]*rbClass{}
			}
			if fr.rbClasses[name] == nil {
				fr.rbClasses[name] = &rbClass{}
			}
			if m[8] >= 0 {
				fr.rbClasses[name].super = line[m[8]:m[9]]
			}
			// class Foo < Bar; end and Struct.new(...) do … opens only when
			// the line does not close it again.
			toks = append(toks, token{pos: m[2], open: &rbFrame{kind: "type", sym: s, name: name}})
			consumed = m[3]
		case rbDef.MatchString(line):
			m := rbDef.FindStringSubmatchIndex(line)
			name := line[m[6]:m[7]]
			vis := ""
			if m[2] >= 0 {
				vis = line[m[2]:m[3]]
			}
			s := &symbol{name: name, kind: model.KindFunction, start: n, end: n, signature: strings.TrimSpace(raw[i]), doc: rubyDoc(raw, i)}
			s.exported = vis != "private" && vis != "protected" && vis != "private_class_method" && (tf == nil || !tf.private || tf.single || m[4] >= 0)
			if cls != "" {
				s.class, s.name, s.kind = cls, cls+"."+name, model.KindMethod
				if tf != nil && tf.sym != nil && tf.sym.root && s.exported {
					s.root = true // a public controller action
				}
			}
			if strings.HasPrefix(name, "test_") {
				s.isTest = true
			}
			fr.symbols = append(fr.symbols, s)
			if m[10] < 0 { // not an endless def x = expr
				toks = append(toks, token{pos: m[0], open: &rbFrame{kind: "def", sym: s}})
			}
			consumed = m[1]
		default:
			if m := rbOpenStart.FindStringSubmatchIndex(line); m != nil {
				toks = append(toks, token{pos: m[2], open: &rbFrame{kind: "block"}})
				consumed = m[3]
			}
		}
		if tf != nil && innerDef() == nil {
			switch strings.TrimSpace(line) {
			case "private", "protected":
				tf.private = true
			case "public":
				tf.private = false
			}
			if m := rbBeforeAction.FindStringSubmatch(raw[i]); m != nil && fr.rbClasses[cls] != nil {
				c := fr.rbClasses[cls]
				if strings.HasPrefix(m[1], "skip") {
					c.skip = append(c.skip, rbCallbacks(m[2])...)
				} else {
					c.before = append(c.before, rbCallbacks(m[2])...)
				}
			}
			for _, m := range rbSymbolRef.FindAllStringSubmatch(line, -1) {
				refs[cls+"."+m[1]] = true // before_action :load_user, validate :check
			}
		}
		rest := line
		if consumed > 0 {
			rest = strings.Repeat(" ", consumed) + line[consumed:]
		}
		for _, m := range rbOpenMid.FindAllStringSubmatchIndex(rest, -1) {
			// After an operator (x = if, html << if, foo(case) the keyword
			// opens a block; after a value (return if, save unless) it is
			// a modifier. ? and ! end method names (save! if x).
			before := strings.TrimRight(rest[:m[2]], " \t")
			if before == "" || !strings.ContainsRune("=(,|&<>+-*/%:[{", rune(before[len(before)-1])) || strings.HasSuffix(before, "->") {
				continue
			}
			toks = append(toks, token{pos: m[2], open: &rbFrame{kind: "block"}})
		}
		for _, m := range rbDefMid.FindAllStringSubmatchIndex(rest, -1) {
			if m[2] < 0 { // not an endless def
				toks = append(toks, token{pos: m[0], open: &rbFrame{kind: "block"}})
			}
		}
		loopKw := false
		if m := rbOpenStart.FindStringSubmatch(line); m != nil && (m[1] == "while" || m[1] == "until" || m[1] == "for") {
			loopKw = true // while x do: the do belongs to the while
		}
		for _, loc := range rbWord.FindAllStringIndex(rest, -1) {
			w := rest[loc[0]:loc[1]]
			if w != "do" && w != "end" {
				continue
			}
			if loc[0] > 0 && (rest[loc[0]-1] == '.' || rest[loc[0]-1] == ':' || rest[loc[0]-1] == '@') {
				continue // range.end, :end, @end
			}
			if loc[1] < len(rest) && rest[loc[1]] == ':' && (loc[1]+1 >= len(rest) || rest[loc[1]+1] != ':') {
				continue // end: as a hash key
			}
			if w == "do" {
				if loopKw {
					loopKw = false
					continue
				}
				f := &rbFrame{kind: "block"}
				if routesFile {
					f.prefix = rbRoutePrefix(raw[i])
					if m := rbNamespace.FindStringSubmatch(raw[i]); m != nil {
						f.module = rbCamel(m[1])
					} else if m := rbRouteModule.FindStringSubmatch(raw[i]); m != nil {
						f.module = rbCamel(m[1]) // scope module: :x do, resources :y, module: :x do
					}
				}
				toks = append(toks, token{pos: loc[0], open: f})
				continue
			}
			toks = append(toks, token{pos: loc[0]})
		}
		slices.SortStableFunc(toks, func(a, b token) int { return a.pos - b.pos })
		owner := innerDef()
		for _, t := range toks {
			if t.open != nil {
				stack = append(stack, t.open)
				continue
			}
			if len(stack) == 0 {
				fr.partial = true
				continue
			}
			top := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if top.sym != nil {
				top.sym.end = n
			}
		}

		// Calls on this line go to the def that encloses it (or opened on it).
		if owner == nil {
			owner = innerDef()
		}
		var calls []call
		for _, m := range rbDotCall.FindAllStringSubmatch(line, -1) {
			// A chained call keeps ")" as its receiver: of unknown type, so
			// only a repository-unique method name resolves it.
			calls = append(calls, call{recv: strings.TrimLeft(m[1], "@"), name: m[2], line: n})
		}
		inClass := cls != ""
		for _, m := range rbBare.FindAllStringSubmatchIndex(line, -1) {
			w := line[m[2]:m[3]]
			after := strings.TrimLeft(line[m[3]:], " ")
			if rbKeywords[w] || strings.HasPrefix(after, "=") && !strings.HasPrefix(after, "==") && !strings.HasPrefix(after, "=~") ||
				strings.HasPrefix(after, ":") && !strings.HasPrefix(after, "::") || owner != nil && owner.start == n {
				continue
			}
			recv := ""
			if inClass {
				recv = "self" // a bare call in a class is a call on self
			}
			calls = append(calls, call{recv: recv, name: w, line: n})
		}
		if owner != nil {
			owner.calls = append(owner.calls, calls...)
		} else {
			fr.topCalls = append(fr.topCalls, calls...)
		}
		fr.findings = append(fr.findings, rubyRules(raw[i], line, n, ownerName(owner))...)
		if routesFile || owner == nil {
			fr.routes = append(fr.routes, rubyRoutes(raw[i], line, n, routePrefix, routeModule, routesFile)...)
		}
	}
	if len(stack) > 0 {
		fr.partial = true
		for _, f := range stack {
			if f.sym != nil && f.sym.end < f.sym.start {
				f.sym.end = len(lines)
			}
		}
	}
	for _, s := range fr.symbols {
		if refs[s.name] {
			s.root = true // named by a callback or validation: Rails calls it
		}
		if s.kind == model.KindType {
			continue
		}
		body := strings.Join(lines[max(0, s.start-1):min(len(lines), s.end)], "\n")
		s.complexity = countBranches(body, rbBranches)
	}
	return fr
}

// rbSingular is the naive singular Rails uses for nested ids: stories ->
// story, users -> user, addresses -> address.
func rbSingular(w string) string {
	switch {
	case strings.HasSuffix(w, "ies"):
		return strings.TrimSuffix(w, "ies") + "y"
	case strings.HasSuffix(w, "sses"), strings.HasSuffix(w, "shes"), strings.HasSuffix(w, "ches"), strings.HasSuffix(w, "xes"), strings.HasSuffix(w, "uses"):
		return strings.TrimSuffix(w, "es")
	}
	return strings.TrimSuffix(w, "s")
}

func rbControllerBase(base string) bool {
	return strings.HasSuffix(base, "Controller") || strings.HasSuffix(base, "Controller::Base") || strings.HasSuffix(base, "Controller::API") ||
		base == "Sinatra::Base" || base == "Grape::API" || strings.HasSuffix(base, "Job") || strings.HasSuffix(base, "Mailer")
}

// rubyDoc reads the # comment lines right above line i.
func rubyDoc(raw []string, i int) string {
	var parts []string
	for j := i - 1; j >= 0 && j >= i-20; j-- {
		t := strings.TrimSpace(raw[j])
		if !strings.HasPrefix(t, "#") {
			break
		}
		t = strings.TrimSpace(strings.TrimLeft(t, "#"))
		if t == "" || strings.HasPrefix(t, "frozen_string_literal") || strings.HasPrefix(t, "@") || strings.HasPrefix(t, "rubocop:") {
			continue
		}
		parts = append([]string{t}, parts...)
	}
	return strings.Join(parts, " ")
}

var (
	rbVerbRoute  = regexp.MustCompile(`^\s*(get|post|put|patch|delete|options|head)\s*\(?\s*['"]([^'"]*)['"]`)
	rbRoot       = regexp.MustCompile(`^\s*root\b`)
	rbResources  = regexp.MustCompile(`^\s*(resources|resource)\s*\(?\s*:(\w+)`)
	rbNamespace  = regexp.MustCompile(`^\s*namespace\s*\(?\s*:(\w+)`)
	rbScope      = regexp.MustCompile(`^\s*scope\s*\(?\s*(?:path:\s*)?['"]([^'"]*)['"]`)
	rbOnlyExcept = regexp.MustCompile(`\b(only|except):\s*(?:\[([^\]]*)\]|:(\w+))`)
)

// rbRoutePrefix is the path a Rails routes block adds: namespace :admin,
// scope '/v1', and resources :users do (nested routes under /users/:user_id).
func rbRoutePrefix(raw string) string {
	switch {
	case rbNamespace.MatchString(raw):
		return "/" + rbNamespace.FindStringSubmatch(raw)[1]
	case rbScope.MatchString(raw):
		return "/" + strings.Trim(rbScope.FindStringSubmatch(raw)[1], "/")
	case rbResources.MatchString(raw):
		m := rbResources.FindStringSubmatch(raw)
		if m[1] == "resource" {
			return "/" + m[2]
		}
		return "/" + m[2] + "/:" + rbSingular(m[2]) + "_id"
	}
	return ""
}

// rubyRoutes reads Rails routes (config/routes.rb: verbs, root, resources)
// and Sinatra/Grape verb blocks (get '/x' do) elsewhere.
func rubyRoutes(raw, masked string, line int, prefix, module string, routesFile bool) []route {
	join := func(p string) string {
		return "/" + strings.Trim(strings.TrimSuffix(prefix, "/")+"/"+strings.TrimPrefix(p, "/"), "/")
	}
	// to: "users#show" names UsersController#show, inside a namespace
	// module's controllers (Admin::UsersController).
	if m := rbRouteModule.FindStringSubmatch(raw); m != nil && !strings.Contains(masked, " do") {
		module = strings.TrimPrefix(module+"::"+rbCamel(m[1]), "::") // resources :x, module: :y
	}
	target := func(r route) route {
		if m := rbRouteTo.FindStringSubmatch(raw); m != nil {
			r.recv, r.handler = rbController(module, m[1]), m[2]
		}
		return r
	}
	if m := rbVerbRoute.FindStringSubmatch(raw); m != nil && (routesFile || strings.Contains(masked, " do")) {
		return []route{target(route{kind: model.RouteHTTP, name: strings.ToUpper(m[1]) + " " + join(m[2]), line: line})}
	}
	if !routesFile {
		return nil
	}
	if rbRoot.MatchString(raw) {
		return []route{target(route{kind: model.RouteHTTP, name: "GET " + join(""), line: line})}
	}
	m := rbResources.FindStringSubmatch(raw)
	if m == nil {
		return nil
	}
	base := join(m[2])
	id := base + "/:id"
	if m[1] == "resource" {
		id = base // a singular resource has no id
	}
	actions := []struct{ action, verb, path string }{
		{"index", "GET", base}, {"create", "POST", base}, {"new", "GET", base + "/new"},
		{"show", "GET", id}, {"edit", "GET", id + "/edit"}, {"update", "PATCH", id}, {"update", "PUT", id}, {"destroy", "DELETE", id},
	}
	keep := func(string) bool { return true }
	if o := rbOnlyExcept.FindStringSubmatch(raw); o != nil {
		list := o[2] + "," + o[3]
		named := func(a string) bool { return strings.Contains(list, ":"+a) || o[3] == a }
		keep = func(a string) bool { return named(a) == (o[1] == "only") }
	}
	ctrl := m[2]
	if m[1] == "resource" {
		ctrl += "s" // resource :profile is served by ProfilesController
	}
	if c := rbRouteController.FindStringSubmatch(raw); c != nil {
		ctrl = c[1]
	}
	var out []route
	for _, a := range actions {
		if m[1] == "resource" && a.action == "index" || !keep(a.action) {
			continue
		}
		out = append(out, route{kind: model.RouteHTTP, name: a.verb + " " + a.path, line: line, recv: rbController(module, ctrl), handler: a.action})
	}
	return out
}

var (
	rbRouteTo         = regexp.MustCompile(`(?:\bto:\s*|=>\s*|^\s*root\s*\(?\s*)['"]([\w/]+)#(\w+)['"]`)
	rbRouteModule     = regexp.MustCompile(`\bmodule:\s*[:'"]?(\w+)`)
	rbRouteController = regexp.MustCompile(`\bcontroller:\s*[:'"]([\w/]+)`)
)

// rbController is the controller class a route names: "admin/users" in
// module "Api" is Api::Admin::UsersController.
func rbController(module, path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = rbCamel(p)
	}
	name := strings.Join(parts, "::") + "Controller"
	if module != "" {
		name = module + "::" + name
	}
	return name
}

// rbCamel is user_sessions -> UserSessions.
func rbCamel(s string) string {
	var b strings.Builder
	for _, w := range strings.Split(s, "_") {
		if w != "" {
			b.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	return b.String()
}

var (
	// rbShellCall captures the first argument: a string runs through a
	// shell, an array (IO.popen([cmd, arg])) or several arguments do not.
	rbShellCall = regexp.MustCompile(`(?:(?:^|[^\w.:])(?:system|exec|spawn)|\b(?:Open3\.(?:capture2e?|capture3|popen2e?|popen3|pipeline\w*)|IO\.popen|Kernel\.(?:system|exec|spawn)))\b\s*\(?\s*("(?:[^"\\]|\\.)*"(?:\s*\+)?|'[^']*'\s*\+)`)
	rbBacktick  = regexp.MustCompile("`[^`]*#\\{|%x[({\\[][^)}\\]]*#\\{")
	rbShellVar  = regexp.MustCompile(`(?:^|[^\w.:@])(?:system|exec)\b\s*\(?\s*[a-z_@]\w*\s*\)?\s*$`)
	// rbSQLArg captures the first argument of a query method: the SQL. A
	// value interpolated into a later, bound argument is not SQL.
	rbSQLArg      = regexp.MustCompile(`(?:\.\s*|(?:^|[^\w.:@?!])|::\s*)(?:where|not|find_by_sql|count_by_sql|execute|exec_query|select_all|select_value|select_values|select_rows|order|reorder|group|having|joins|from|pluck|update_all|delete_all|sanitize_sql)\s*\(?\s*("(?:[^"\\]|\\.)*"|'[^']*')(\s*:[^:]|\s*\+)?`)
	rbEval        = regexp.MustCompile(`(?:^|[^\w.:])eval\s*\(|\.\s*(?:instance_eval|class_eval|module_eval)\s*\(\s*[^{&)]|\b(?:send|public_send|__send__)\s*\(\s*params\b|params\[[^\]]+\]\s*\.\s*(?:safe_)?constantize\b`)
	rbDeserialize = regexp.MustCompile(`\bMarshal\.(?:load|restore)\s*\(|\b(?:YAML|Psych)\.unsafe_load(?:_file)?\s*\(|\bOj\.load\s*\([^)]*mode:\s*:object|\bYAML\.load\s*\([^)]*permitted_classes:\s*\[[^\]]*Object`)
	rbParams      = regexp.MustCompile(`\bparams\s*\[|\bparams\.(?:fetch|require|permit|dig)\b|\brequest\.(?:params|query_parameters|body)\b`)
	rbFileCall    = regexp.MustCompile(`\b(?:File|IO|Pathname|FileUtils)\s*(?:\.|::)\s*\w+|\bsend_file\b|\brender\s*\(?\s*file:|(?:^|[^\w.:])open\s*\(`)
	rbRedirect    = regexp.MustCompile(`\bredirect_to\s*\(?\s*(?:params|request\.(?:referer|params))\b`)
	rbTrustAll    = regexp.MustCompile(`VERIFY_NONE|verify_mode:\s*:none|ssl_verify_mode\s*(?:=|:)\s*:none|\bverify:\s*false\b|ssl:\s*\{[^}]*verify:\s*false`)
	rbWeakHash    = regexp.MustCompile(`\bDigest::(?:MD5|SHA1)\b|\bOpenSSL::Digest::(?:MD5|SHA1)\b|OpenSSL::Digest\.new\(\s*['"](?:md5|sha1)['"]`)
	rbWeakCipher  = regexp.MustCompile(`(?i)OpenSSL::Cipher(?:\.new|::Cipher\.new)?\s*\(\s*['"](?:des|des-ede3|des3|rc2|rc4|bf|blowfish)[-\w]*['"]|OpenSSL::Cipher\.new\(\s*['"][^'"]*-ecb['"]|OpenSSL::Cipher::(?:DES|RC4|RC2|BF)\b`)
	rbRandom      = regexp.MustCompile(`(?:^|[^\w.:])rand\s*\(|\bRandom\.(?:rand|new)\b|\.shuffle\b`)
	rbSecret      = regexp.MustCompile(`(?:^|[^\w.:@])([A-Za-z_]\w*)\s*=\s*['"]([^'"]{8,})['"]`)
)

// rubyRules reports Ruby findings on one line (rules/code/*.yaml).
func rubyRules(raw, masked string, line int, owner string) []finding {
	var out []finding
	add := func(rule, detail string) {
		out = append(out, finding{rule: rule, detail: detail, line: line, owner: owner})
	}
	switch {
	case rbShellCall.MatchString(masked) && rbBuiltArg(rbShellCall.FindStringSubmatch(raw)) && !strings.Contains(raw, "Shellwords") && !strings.Contains(raw, "shellescape"):
		add("exec-shell", "a shell runs a command built at run time")
	case rbBacktick.MatchString(raw) && strings.ContainsAny(masked, "`%"):
		add("exec-shell", "backticks run a command built at run time")
	case rbShellVar.MatchString(masked):
		add("exec-variable", "system/exec with a command that is not a literal")
	}
	if m := rbSQLArg.FindStringSubmatch(raw); rbSQLArg.MatchString(masked) && rbBuiltArg(m) && !strings.HasPrefix(strings.TrimSpace(m[2]), ":") {
		add("sql-concat", "a query built by interpolation or concatenation")
	}
	if rbEval.MatchString(masked) {
		add("code-injection", "eval, string class_eval, or send/constantize with request input")
	}
	if rbDeserialize.MatchString(masked) {
		add("deserialize", "Marshal or unsafe YAML loading")
	}
	if rbFileCall.MatchString(masked) && rbParams.MatchString(raw) {
		add("path-from-request", "a filesystem call with request input")
	}
	if rbRedirect.MatchString(masked) {
		add("open-redirect", "redirect_to a URL from the request")
	}
	if rbTrustAll.MatchString(raw) {
		add("tls-insecure", "certificate verification disabled")
	}
	if rbWeakHash.MatchString(masked) && jvmSecretWord.MatchString(raw) {
		add("weak-hash", "MD5 or SHA-1 for a secret value")
	}
	if rbWeakCipher.MatchString(raw) {
		add("weak-cipher", "a broken cipher or ECB mode")
	}
	if rbRandom.MatchString(masked) && jvmSecretWord.MatchString(raw) {
		add("insecure-rand", "rand for a secret value; use SecureRandom")
	}
	if m := rbSecret.FindStringSubmatch(raw); m != nil && looksSecret(m[1], m[2]) {
		add("hardcoded-secret", fmt.Sprintf("%s is assigned a %d-character literal", m[1], len(m[2])))
	}
	return out
}

// rbBuiltArg reports whether a captured first argument (m[1], with what
// follows it in m[2]) is built at run time: interpolated or concatenated.
func rbBuiltArg(m []string) bool {
	if m == nil {
		return false
	}
	return strings.Contains(m[1], "#{") && strings.HasPrefix(m[1], `"`) || strings.HasSuffix(strings.TrimSpace(m[1]), "+") ||
		len(m) > 2 && strings.Contains(m[2], "+")
}

// rubyTags maps Ruby library calls to security tags.
func rubyTags(recv, n string, set map[string]bool) {
	switch {
	case recv == "Open3" || recv == "" && (n == "system" || n == "spawn") || recv == "IO" && n == "popen":
		set["exec"] = true
	case n == "find_by_sql" || n == "exec_query" || recv == "connection" && n == "execute" || n == "select_all":
		set["sql"] = true
	case recv == "Net::HTTP" || recv == "HTTP" || recv == "Faraday" || recv == "HTTParty" || recv == "RestClient" || recv == "URI" && n == "open":
		set["http"] = true
	case recv == "File" || recv == "FileUtils" || recv == "Dir" || recv == "IO" && n != "popen" || n == "send_file":
		set["filesystem"] = true
	case recv == "ENV":
		set["env"] = true
	case recv == "SecureRandom" || recv == "BCrypt::Password" || recv == "OpenSSL::HMAC" || recv == "OpenSSL::Cipher" || recv == "Digest::SHA256":
		set["crypto"] = true
	case recv == "Marshal" || recv == "YAML" && strings.HasPrefix(n, "unsafe") || recv == "Oj" && n == "load":
		set["deserialize"] = true
	}
}

// rubyResolveRequire maps require_relative './x' to a file of the
// repository, and require 'x' to one in the usual load paths (lib/, spec/,
// test/ of the file's project).
func rubyResolveRequire(files map[string]*parsedFile, from, spec string) *parsedFile {
	var cands []string
	if strings.HasPrefix(spec, ".") {
		p := path.Join(path.Dir(from), spec)
		cands = []string{p, p + ".rb"}
	} else {
		root := ""
		for _, dir := range []string{"/lib/", "/spec/", "/test/", "/app/"} {
			if i := strings.Index("/"+from, dir); i >= 0 {
				root = strings.TrimSuffix(from[:i], "/")
				break
			}
		}
		for _, dir := range []string{"lib", "spec", "test"} {
			cands = append(cands, path.Join(root, dir, spec+".rb"))
		}
	}
	for _, cand := range cands {
		if f, ok := files[cand]; ok {
			return f
		}
	}
	return nil
}

// maskRuby blanks string and literal contents, comments, =begin blocks and
// heredoc bodies, keeping line structure.
func maskRuby(src string) string {
	b := []byte(src)
	out := make([]byte, len(b))
	copy(out, b)
	blank := func(i int) {
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	closer := map[byte]byte{'(': ')', '[': ']', '{': '}', '<': '>'}
	var heredocs []string // labels whose bodies start on the next line
	prevSig := func(i int) byte {
		for j := i - 1; j >= 0; j-- {
			if b[j] == ' ' || b[j] == '\t' {
				continue
			}
			return b[j]
		}
		return '\n'
	}
	prevWord := func(i int) string {
		j := i - 1
		for j >= 0 && (b[j] == ' ' || b[j] == '\t') {
			j--
		}
		k := j
		for k >= 0 && isIdent(rune(b[k])) {
			k--
		}
		return string(b[k+1 : j+1])
	}
	// skip blanks a delimited literal from its opening delimiter at i and
	// returns the index of its closing delimiter.
	skip := func(i int, open byte) int {
		cl, nest := open, false
		if c, ok := closer[open]; ok {
			cl, nest = c, true
		}
		depth, interp := 1, 0
		j := i + 1
		for ; j < len(b); j++ {
			switch {
			case b[j] == '\\':
				blank(j)
				j++
				if j < len(b) {
					blank(j)
				}
				continue
			case (open == '"' || open == '`') && b[j] == '#' && j+1 < len(b) && b[j+1] == '{':
				interp++ // "#{h["k"]}": quotes inside an interpolation do not close
				blank(j)
				j++
			case interp > 0 && b[j] == '{':
				interp++
			case interp > 0 && b[j] == '}':
				interp--
			case interp > 0:
			case nest && b[j] == open:
				depth++
			case b[j] == cl:
				depth--
			}
			if depth == 0 {
				break
			}
			blank(j)
		}
		return j
	}
	// operand reports whether position i starts an operand (after an
	// operator), and argStart whether it starts the first argument of a call
	// without parentheses (puts %w[a], assert_match /x/): a space before and
	// none after, as Ruby itself decides.
	operand := func(i int) bool {
		p := prevSig(i)
		return !isIdent(rune(p)) && p != ')' && p != ']' && p != '}'
	}
	argStart := func(i int) bool {
		return i > 0 && i+2 < len(b) && (b[i-1] == ' ' || b[i-1] == '\t') && isIdent(rune(prevSig(i))) && b[i+1] != ' ' && b[i+2] != ' ' && !rbKeywordValue[prevWord(i)]
	}
	lineStart := true
	for i := 0; i < len(b); i++ {
		c := b[i]
		if lineStart {
			lineStart = false
			if strings.HasPrefix(src[i:], "=begin") {
				for i < len(b) && !(strings.HasPrefix(src[i:], "\n=end")) {
					blank(i)
					i++
				}
				if i < len(b) {
					i++ // keep the newline, blank =end
					for i < len(b) && b[i] != '\n' {
						blank(i)
						i++
					}
				}
				i--
				continue
			}
			if strings.HasPrefix(src[i:], "__END__") {
				for ; i < len(b); i++ {
					blank(i)
				}
				break
			}
		}
		switch {
		case c == '\n':
			lineStart = true
			for len(heredocs) > 0 {
				label := heredocs[0]
				heredocs = heredocs[1:]
				j := i + 1
				for j < len(b) {
					end := strings.IndexByte(src[j:], '\n')
					if end < 0 {
						end = len(src) - j
					}
					if strings.TrimSpace(src[j:j+end]) == label {
						j += end
						break
					}
					for k := j; k < j+end; k++ {
						blank(k)
					}
					j += end + 1
				}
				i = min(j, len(b)) - 1
			}
		case c == '#':
			for i < len(b) && b[i] != '\n' {
				blank(i)
				i++
			}
			i--
		case c == '"' || c == '\'' || c == '`':
			i = skip(i, c)
		case c == ':' && i+1 < len(b) && (b[i+1] == '"' || b[i+1] == '\''):
			i = skip(i+1, b[i+1])
		case c == '%' && i+2 < len(b) && strings.IndexByte("qQwWiIxrs", b[i+1]) >= 0 && strings.IndexByte("([{<|!/^", b[i+2]) >= 0:
			i = skip(i+2, b[i+2])
		case c == '%' && i+1 < len(b) && strings.IndexByte("([{<|!", b[i+1]) >= 0 && (operand(i) || argStart(i)):
			i = skip(i+1, b[i+1])
		case c == '$' && i+1 < len(b) && strings.IndexByte("`'\"!@&+~=/\\,;.<>_0*$?:", b[i+1]) >= 0:
			i++ // a special global ($', $`, $~): its punctuation opens nothing
		case c == '<' && strings.HasPrefix(src[i:], "<<") && i+2 < len(b) && (b[i+2] == '~' || b[i+2] == '-' || b[i+2] >= 'A' && b[i+2] <= 'Z' || b[i+2] == '\'' || b[i+2] == '"'):
			j := i + 2
			if b[j] == '~' || b[j] == '-' {
				j++
			}
			quote := byte(0)
			if j < len(b) && (b[j] == '\'' || b[j] == '"') {
				quote = b[j]
				j++
			}
			k := j
			for k < len(b) && isIdent(rune(b[k])) {
				k++
			}
			if k > j && (quote != 0 || src[j] >= 'A' && src[j] <= 'Z') {
				heredocs = append(heredocs, src[j:k])
				if quote != 0 && k < len(b) && b[k] == quote {
					k++
				}
				i = k - 1
			}
		case c == '/':
			p := prevSig(i)
			w := prevWord(i)
			if strings.IndexByte("(,=~!|&{[;\n", p) >= 0 || argStart(i) && i+1 < len(b) && b[i+1] != '=' || w == "when" || w == "if" || w == "unless" || w == "and" || w == "or" || w == "return" || w == "split" || w == "match" || w == "scan" || w == "gsub" || w == "sub" {
				j := i + 1
				for j < len(b) && b[j] != '/' && b[j] != '\n' {
					if b[j] == '\\' {
						blank(j)
						j++
					}
					if j < len(b) {
						blank(j)
					}
					j++
				}
				if j < len(b) && b[j] == '/' {
					i = j
				}
			}
		}
	}
	return string(out)
}
