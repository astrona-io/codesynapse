package lang

import (
	"regexp"
	"slices"
	"strings"

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
func jsRoutes(src string, fns map[string]bool) []route {
	var out []route
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") {
			continue
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

// djangoRoutes reads the routes of a urls.py file.
func djangoRoutes(src string) []route {
	var out []route
	for i, l := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
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
	return out
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
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				if a := strings.TrimSpace(s[start:i]); a != "" {
					out = append(out, a)
				}
				return out
			}
			depth--
		case ',':
			if depth == 0 {
				if a := strings.TrimSpace(s[start:i]); a != "" {
					out = append(out, a)
				}
				start = i + 1
			}
		}
	}
	if a := strings.TrimSpace(s[start:]); a != "" {
		out = append(out, a)
	}
	return out
}
