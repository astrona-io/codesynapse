package lang

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Entry points of Python and JavaScript/TypeScript code (docs/PLAN.md,
// security roadmap item 4; internal/ingest/golang/routes.go does Go):
// routes a web framework serves, CLI commands and MCP tools, found by the
// shape of their registration in the source.

// route is one registration found in a file. handler is the function as
// the registration names it, recv its qualifier when written as one
// (handlers::list, UserController@show); link() resolves both through the
// file's imports. Both are "" for inline handlers.
type route struct {
	kind    string
	name    string
	line    int
	handler string
	recv    string
	guards  []string
	// bare marks a handler named without its class ('index'), which a
	// controller scope supplies (Laravel's Route::controller(X::class)).
	bare bool
}

var (
	// @app.route("/x", methods=["POST"]), @router.get("/items/{id}"),
	// @app.api_route(path="/x"), @app.websocket("/ws"): Flask, FastAPI,
	// Starlette, Quart, Sanic.
	pyRouteDec = regexp.MustCompile(`^\s*@([\w.]+?)\.(route|get|post|put|patch|delete|head|options|api_route|websocket)\(\s*(?:(?:path|rule)\s*=\s*)?[rbuf]*(['"])(/[^'"]*)?['"]`)
	pyMethods  = regexp.MustCompile(`methods\s*=\s*[\[(]([^\])]*)[\])]`)
	pyWord     = regexp.MustCompile(`['"](\w+)['"]`)
	// @click.command(), @app.command("name"), @cli.command(name="x"): click, Typer.
	pyCommandDec = regexp.MustCompile(`^\s*@([\w.]+?)\.command\(\s*(?:(?:name\s*=\s*)?['"]([\w:-]+)['"])?`)
	// @mcp.tool(), @server.tool(name="x"): FastMCP and the MCP Python SDK.
	pyToolDec = regexp.MustCompile(`^\s*@([\w.]+?)\.tool\(\s*(?:(?:name\s*=\s*)?['"]([\w.-]+)['"])?`)

	// app.get('/x', auth, handler), router.post("/x", (req, res) => …):
	// Express, Fastify, Hono, Koa routers. Counted only when the last
	// argument is a function, so HTTP clients (axios.get('/x', {params}))
	// are not routes.
	jsRoute      = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*\.\s*(get|post|put|patch|delete|head|options|all)\s*\(\s*(['"` + "`" + `])(/[^'"` + "`" + `]*)['"` + "`" + `]\s*,(.*)$`)
	jsIdent      = regexp.MustCompile(`^[A-Za-z_$][\w$.]*$`)
	jsInlineFunc = regexp.MustCompile(`=>|\bfunction\b`)
	// app.use(requireAuth), router.use(passport.authenticate('jwt')): only
	// middleware whose name says it checks access counts as a guard.
	jsUse      = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*\.\s*use\s*\(\s*([^'"` + "`" + `\s].*)$`)
	jsAccessMW = accessCheck
	// program.command('sync <dir>') … .action(syncAll): commander.
	jsCommand = regexp.MustCompile(`\.command\(\s*['"` + "`" + `]([\w:.-]+)`)
	jsAction  = regexp.MustCompile(`\.action\(\s*([A-Za-z_$][\w$.]*)\s*\)`)
	// server.tool("name", …), server.registerTool("name", …): the MCP TS SDK.
	jsTool = regexp.MustCompile(`\.\s*(?:tool|registerTool)\s*\(\s*(['"])([\w.-]+)['"]`)
	// Clients that take a path and options but serve nothing.
	jsClients = []string{"axios", "fetch", "$fetch", "ofetch", "ky", "got", "superagent", "http", "https", "request", "client", "cy"}
)

// pyRoutesFor reads the decorators above def name: the routes, commands and
// tools it serves and the other decorators as guards.
func pyRoutesFor(decorators []string, names []string, line int, name string) []route {
	var out []route
	var guards []string
	for i, d := range decorators {
		switch {
		case pyRouteDec.MatchString(d):
			m := pyRouteDec.FindStringSubmatch(d)
			path := m[4]
			if path == "" {
				continue // not a path literal (patch.object("module.x"))
			}
			var methods []string
			switch m[2] {
			case "route", "api_route":
				if mm := pyMethods.FindStringSubmatch(d); mm != nil {
					for _, w := range pyWord.FindAllStringSubmatch(mm[1], -1) {
						methods = append(methods, strings.ToUpper(w[1]))
					}
				}
				if len(methods) == 0 && m[2] == "route" {
					methods = []string{"GET"} // Flask's default
				}
			case "websocket":
				methods = []string{"WS"}
			default:
				methods = []string{strings.ToUpper(m[2])}
			}
			if len(methods) == 0 {
				methods = []string{""}
			}
			for _, meth := range methods {
				out = append(out, route{kind: model.RouteHTTP, name: strings.TrimSpace(meth + " " + path), line: line, handler: name})
			}
		case pyCommandDec.MatchString(d):
			m := pyCommandDec.FindStringSubmatch(d)
			cmd := m[2]
			if cmd == "" {
				cmd = strings.ReplaceAll(lastPart(name), "_", "-")
			}
			out = append(out, route{kind: model.RouteCLI, name: cmd, line: line, handler: name})
		case pyToolDec.MatchString(d):
			m := pyToolDec.FindStringSubmatch(d)
			tool := m[2]
			if tool == "" {
				tool = lastPart(name)
			}
			out = append(out, route{kind: model.RouteMCP, name: tool, line: line, handler: name})
		default:
			if !onlyBuiltinDecorators([]string{names[i]}) {
				guards = append(guards, names[i])
			}
		}
	}
	for i := range out {
		out[i].guards = guards
	}
	return out
}

// jsRoutes finds route and tool registrations in JavaScript or TypeScript
// source; fns are the file's function names, for handlers passed by name.
// router.use(requireAuth) guards the routes registered on router after it,
// and with commander, program.command('x').action(fn) is a CLI command.
func jsRoutes(src string, fns map[string]bool, commander bool) []route {
	var out []route
	lines := strings.Split(src, "\n")
	used := map[string][]string{} // receiver -> access middleware applied with use()
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") {
			continue
		}
		if m := jsUse.FindStringSubmatch(l); m != nil {
			for _, a := range splitArgs(m[2]) {
				name := strings.SplitN(a, "(", 2)[0]
				if jsIdent.MatchString(name) && jsAccessMW.MatchString(name) {
					used[m[1]] = append(used[m[1]], a)
				}
			}
		}
		if commander {
			if m := jsCommand.FindStringSubmatch(l); m != nil {
				r := route{kind: model.RouteCLI, name: m[1], line: i + 1}
				for j := i; j < len(lines) && j <= i+6; j++ {
					if j > i && strings.Contains(lines[j], ".command(") {
						break
					}
					if a := jsAction.FindStringSubmatch(lines[j]); a != nil {
						r.handler = a[1]
						if k := strings.LastIndexByte(a[1], '.'); k > 0 {
							r.recv, r.handler = a[1][:k], a[1][k+1:]
						}
						break
					}
				}
				out = append(out, r)
			}
		}
		if m := jsTool.FindStringSubmatch(l); m != nil {
			out = append(out, route{kind: model.RouteMCP, name: m[2], line: i + 1})
		}
		m := jsRoute.FindStringSubmatch(l)
		if m == nil || slices.Contains(jsClients, m[1]) {
			continue
		}
		rest := strings.TrimSpace(m[5])
		// A call spread over lines: the handler follows on the next ones.
		for j := i + 1; j < len(lines) && j <= i+3 && !strings.Contains(rest, ")") && !jsInlineFunc.MatchString(rest); j++ {
			rest += " " + strings.TrimSpace(lines[j])
		}
		args := splitArgs(rest)
		if len(args) == 0 {
			continue
		}
		last := args[len(args)-1]
		r := route{kind: model.RouteHTTP, name: strings.TrimSpace(httpVerb(m[2]) + " " + m[4]), line: i + 1}
		switch {
		case jsInlineFunc.MatchString(last) || jsInlineFunc.MatchString(rest) && !jsIdent.MatchString(last):
			// inline handler: the route is real, its body is not a symbol
		case jsIdent.MatchString(last):
			r.handler = last // a function here, or imported: link() decides
			if i := strings.LastIndexByte(last, '.'); i > 0 && !fns[last] {
				r.recv, r.handler = last[:i], last[i+1:] // users.list, controllers.users.list
			}
		default:
			continue // options object or anything else: not a registration
		}
		r.guards = append(r.guards, used[m[1]]...)
		for _, a := range args[:len(args)-1] {
			if jsIdent.MatchString(a) || strings.HasSuffix(a, ")") && jsIdent.MatchString(strings.SplitN(a, "(", 2)[0]) {
				r.guards = append(r.guards, a)
			}
		}
		out = append(out, r)
	}
	return out
}

// djangoPath is a Django URLconf entry: path("users/<int:id>/", views.detail)
// or re_path(r"^x/$", View.as_view()); include(...) adds no handler.
var djangoPath = regexp.MustCompile(`\b(?:re_)?path\(\s*r?['"]([^'"]*)['"]\s*,\s*([A-Za-z_][\w.]*)`)

var djangoInclude = regexp.MustCompile(`\b(?:re_)?path\(\s*r?['"]([^'"]*)['"]\s*,\s*include\(\s*\(?\s*['"]([\w.]+)['"]`)

// djangoRoutes reads the routes of a urls.py file, and the urls modules it
// mounts with include() under a prefix.
func djangoRoutes(src string) ([]route, []routeInclude) {
	var out []route
	var incs []routeInclude
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if m := djangoInclude.FindStringSubmatch(l); m != nil {
			incs = append(incs, routeInclude{prefix: strings.TrimPrefix(m[1], "^"), module: m[2]})
			continue
		}
		m := djangoPath.FindStringSubmatch(l)
		if m == nil || m[2] == "include" {
			continue
		}
		r := route{kind: model.RouteHTTP, name: "/" + strings.TrimPrefix(strings.TrimPrefix(m[1], "^"), "/"), line: i + 1}
		ref := strings.TrimSuffix(m[2], ".as_view") // a class-based view: the class handles it
		r.handler = ref
		if j := strings.LastIndexByte(ref, '.'); j > 0 {
			r.recv, r.handler = ref[:j], ref[j+1:]
		}
		out = append(out, r)
	}
	return out, incs
}

func httpVerb(m string) string {
	if m == "all" {
		return ""
	}
	return strings.ToUpper(m)
}

// splitArgs splits a call's remaining arguments at top-level commas, up to
// its closing parenthesis; nested brackets stay whole.
func splitArgs(s string) []string {
	var out []string
	for _, sp := range argSpans(s) {
		out = append(out, s[sp[0]:sp[1]])
	}
	return out
}

// argSpans is splitArgs as byte offsets into s, so a caller can read the
// same argument in an unmasked copy of s (masking keeps offsets).
func argSpans(s string) [][2]int {
	var out [][2]int
	add := func(from, to int) {
		seg := s[from:to]
		trimmed := strings.TrimSpace(seg)
		if trimmed == "" {
			return
		}
		lead := len(seg) - len(strings.TrimLeftFunc(seg, unicode.IsSpace))
		out = append(out, [2]int{from + lead, from + lead + len(trimmed)})
	}
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				add(start, i)
				return out
			}
			depth--
		case ',':
			if depth == 0 {
				add(start, i)
				start = i + 1
			}
		}
	}
	add(start, len(s))
	return out
}

// accessCheck matches names of middleware, callbacks and decorators that
// check who may call (authenticate_user!, login_required, requireAuth),
// as opposed to ones that load data or set headers (set_post, never_cache).
var accessCheck = regexp.MustCompile(`(?i)auth|login|logged_?in|signed_?in|permission|admin|staff|superuser|role|access|authoriz|verif|token|csrf_?protect|protect|guard|acl|jwt|passport|session|require_?(?:user|login|admin|role|permission|functional|account|owner|member)|ensure_?(?:user|admin|owner|member|logged)|doorkeeper|can[_: ]`)

// AccessGuards keeps the names that look like access checks.
func AccessGuards(names []string) []string {
	var out []string
	for _, n := range names {
		if accessCheck.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

// routeScope is a block of lines whose routes share a path prefix and
// guards (a Laravel route group, an Express router after router.use).
type routeScope struct {
	start, end int
	prefix     string
	guards     []string
	recv       string // the controller of bare handlers inside
}

// applyScopes prefixes and guards the routes registered inside scopes,
// outer scopes first.
func applyScopes(routes []route, scopes []routeScope) []route {
	for i := range routes {
		r := &routes[i]
		verb, path, hasVerb := strings.Cut(r.name, " ")
		if !hasVerb {
			verb, path = "", r.name
		}
		prefix := ""
		var guards []string
		for _, s := range scopes {
			if r.line >= s.start && r.line <= s.end {
				prefix = strings.TrimSuffix(prefix, "/") + "/" + strings.Trim(s.prefix, "/")
				guards = append(guards, AccessGuards(s.guards)...) // a group's web/throttle middleware is not a check
				if r.bare && s.recv != "" {
					r.recv = s.recv
				}
			}
		}
		if r.bare && r.recv == "" {
			r.handler = "" // a method name with no controller to find it on
		}
		if prefix != "" && strings.Trim(prefix, "/") != "" {
			path = "/" + strings.Trim(strings.TrimSuffix(prefix, "/")+"/"+strings.TrimPrefix(path, "/"), "/")
			r.name = strings.TrimSpace(verb + " " + path)
		}
		r.guards = append(guards, r.guards...)
	}
	return routes
}

var (
	nestVerb  = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Patch": "PATCH", "Delete": "DELETE", "Head": "HEAD", "Options": "OPTIONS", "All": ""}
	nestGuard = map[string]bool{"UseGuards": true, "Roles": true, "Public": true, "Auth": true, "Permissions": true, "UseInterceptors": false}
	jsString  = regexp.MustCompile(`['"` + "`" + `]([^'"` + "`" + `]*)['"` + "`" + `]`)
)

// nestRoutes reads NestJS routes: @Controller('users') on the class and
// @Get(':id') on the method, with @UseGuards/@Roles/@Public as guards.
func nestRoutes(classDecs, methodDecs []string, handler string, line int) []route {
	prefix, controller := "", false
	var guards []string
	name := func(d string) string {
		d = strings.TrimPrefix(strings.TrimSpace(d), "@")
		if i := strings.IndexAny(d, "( "); i >= 0 {
			d = d[:i]
		}
		return d
	}
	for _, d := range classDecs {
		switch n := name(d); {
		case n == "Controller":
			controller = true
			if m := jsString.FindStringSubmatch(d); m != nil {
				prefix = m[1]
			}
		case nestGuard[n]:
			guards = append(guards, clipGuard(d))
		}
	}
	if !controller {
		return nil
	}
	var out []route
	var methodGuards []string
	for _, d := range methodDecs {
		if n := name(d); nestGuard[n] {
			methodGuards = append(methodGuards, clipGuard(d))
		}
	}
	for _, d := range methodDecs {
		verb, ok := nestVerb[name(d)]
		if !ok {
			continue
		}
		path := ""
		if m := jsString.FindStringSubmatch(d); m != nil {
			path = m[1]
		}
		full := "/" + strings.Trim(strings.TrimSuffix(prefix, "/")+"/"+strings.TrimPrefix(path, "/"), "/")
		out = append(out, route{kind: model.RouteHTTP, name: strings.TrimSpace(verb + " " + full), line: line, handler: handler,
			guards: append(slices.Clip(guards), methodGuards...)})
	}
	return out
}
