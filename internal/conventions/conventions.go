// Package conventions derives a repository's house style from its graph
// (docs/PLAN.md, "Project conventions"): how tests assert, how errors are
// made, which logger and frameworks the code uses. An agent that knows them
// writes code that matches the repository without reading examples first.
//
// Everything is counted at ingest, from the typed external calls of the Go
// extractor (which the graph does not keep) and the external imports of
// every language, so each statement carries the evidence it rests on. The
// statements name packages and counts only, never source.
package conventions

import (
	"cmp"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Input is what an ingest knows.
type Input struct {
	Graph *model.Graph
	// GoCalls maps the full name of each called out-of-repo Go function to
	// its callers, one entry per call site (golang.Result.ExternalCalls).
	GoCalls map[string][]string
	// Imports maps external import paths to the importing node IDs: Go
	// packages, or files for the other languages.
	Imports map[string][]string
	// Module is the root Go module, left out of package paths shown.
	Module string
}

// MaxStatements bounds an answer's conventions.
const MaxStatements = 8

// Analyze returns the conventions found, as short statements with their
// evidence, most useful first.
func Analyze(in Input) []string {
	a := analysis{in: in, tests: map[string]bool{}, nodes: map[string]model.Node{}}
	for _, n := range in.Graph.Nodes() {
		a.nodes[n.ID] = n
		if n.Kind == model.KindFile {
			a.paths = append(a.paths, n.File)
		}
		if n.IsTest && (n.Kind == model.KindFunction || n.Kind == model.KindMethod) {
			a.tests[n.ID] = true
		}
	}
	var out []string
	out = append(out, a.goTests()...)
	out = append(out, a.importTests()...)
	out = append(out, a.goErrors()...)
	out = append(out, a.logging()...)
	out = append(out, a.frameworks()...)
	return out[:min(len(out), MaxStatements)]
}

type analysis struct {
	in    Input
	tests map[string]bool
	nodes map[string]model.Node
	paths []string // indexed files
}

// goPackage splits a Go function's full name into its package path:
// "fmt.Errorf" is fmt, "(*log/slog.Logger).Info" is log/slog.
func goPackage(full string) string {
	full = strings.TrimPrefix(strings.TrimPrefix(full, "("), "*")
	if i := strings.Index(full, ")"); i >= 0 {
		full = full[:i]
	}
	slash := strings.LastIndexByte(full, '/')
	dot := strings.IndexByte(full[slash+1:], '.')
	if dot < 0 {
		return full
	}
	return full[:slash+1+dot]
}

// under reports whether package or import path p is pkg or inside it.
func under(p, pkg string) bool { return p == pkg || strings.HasPrefix(p, pkg+"/") }

func (a analysis) short(pkg string) string {
	if a.in.Module != "" {
		if rest, ok := strings.CutPrefix(pkg, a.in.Module+"/"); ok {
			return rest
		}
	}
	return pkg
}

// goCalls counts call sites into packages matching any of pkgs, all or
// from tests only.
func (a analysis) goCalls(fromTests bool, pkgs ...string) int {
	n := 0
	for full, callers := range a.in.GoCalls {
		p := goPackage(full)
		if !slices.ContainsFunc(pkgs, func(pkg string) bool { return under(p, pkg) }) {
			continue
		}
		for _, c := range callers {
			if !fromTests || a.tests[c] {
				n++
			}
		}
	}
	return n
}

// testFuncs are the Go test functions (TestX), the ones a convention about
// tests is measured against.
func (a analysis) testFuncs() []string {
	var out []string
	for id := range a.tests {
		if n := a.nodes[id]; n.Language == "" || n.Language == "go" {
			if strings.HasPrefix(n.Name, "Test") {
				out = append(out, id)
			}
		}
	}
	return out
}

var goAssertLibs = []struct{ label, pkg string }{
	{"testify", "github.com/stretchr/testify"},
	{"gotest.tools", "gotest.tools"},
	{"gomega", "github.com/onsi/gomega"},
	{"quicktest", "github.com/frankban/quicktest"},
	{"is", "github.com/matryer/is"},
	{"go-cmp", "github.com/google/go-cmp"},
}

func (a analysis) goTests() []string {
	funcs := a.testFuncs()
	if len(funcs) == 0 {
		return nil
	}
	var out []string
	// Assertions: libraries, then the repository's own helper packages
	// (called almost only from tests).
	var libs []string
	for _, l := range goAssertLibs {
		if n := a.goCalls(true, l.pkg); n > 0 {
			libs = append(libs, fmt.Sprintf("%s (%s)", l.label, plural(n, "call")))
		}
	}
	helpers := a.helperPackages()
	switch {
	case len(helpers) > 0 && len(libs) == 0:
		out = append(out, fmt.Sprintf("tests: assert with the repository's own helpers %s, not a library like testify", strings.Join(helpers, ", ")))
	case len(helpers) > 0:
		out = append(out, fmt.Sprintf("tests: assert with %s and the repository's helpers %s", strings.Join(libs, ", "), strings.Join(helpers, ", ")))
	case len(libs) > 0:
		out = append(out, "tests: assert with "+strings.Join(libs, ", "))
	default:
		out = append(out, "tests: the standard library only (t.Errorf, t.Fatalf), no assertion library")
	}
	sub := 0
	runs := map[string]bool{}
	for _, c := range a.in.GoCalls["(*testing.T).Run"] {
		runs[c] = true
	}
	for _, id := range funcs {
		if runs[id] {
			sub++
		}
	}
	if sub*10 >= len(funcs) { // a tenth or more: a habit worth following
		out = append(out, fmt.Sprintf("tests: table-driven with t.Run subtests in %d of %d test functions", sub, len(funcs)))
	}
	fuzz := 0
	for id := range a.tests {
		if strings.HasPrefix(a.nodes[id].Name, "Fuzz") {
			fuzz++
		}
	}
	if fuzz > 0 {
		out = append(out, fmt.Sprintf("tests: %s (func FuzzX): parsers and decoders get one", plural(fuzz, "fuzz test")))
	}
	return out
}

// testSupport reports a package that exists for tests (storetest,
// accountstest, testutil, fixtures): its calls count as test calls.
func testSupport(pkg string) bool {
	last := pkg[strings.LastIndexByte(pkg, '/')+1:]
	return strings.HasSuffix(last, "test") || strings.Contains(pkg, "/testutil") || last == "testdata" || last == "fixtures" || last == "testing"
}

// helperPackages are in-repository packages that tests of at least three
// other packages call ten times or more, and other code hardly at all. A
// test calling into the package it tests does not count.
func (a analysis) helperPackages() []string {
	fromTests, fromCode := map[string]int{}, map[string]int{}
	testers := map[string]map[string]bool{}
	for _, e := range a.in.Graph.Edges() {
		if e.Kind != model.EdgeCalls {
			continue
		}
		to, ok := a.nodes[e.To]
		from := a.nodes[e.From]
		if !ok || to.IsTest || to.Package == "" || to.Kind == model.KindExternal || from.Package == to.Package {
			continue
		}
		if a.tests[e.From] || testSupport(from.Package) {
			fromTests[to.Package]++
			if testers[to.Package] == nil {
				testers[to.Package] = map[string]bool{}
			}
			testers[to.Package][from.Package] = true
		} else {
			fromCode[to.Package]++
		}
	}
	type pkg struct {
		name  string
		calls int
	}
	var found []pkg
	for p, n := range fromTests {
		if n >= 10 && len(testers[p]) >= 3 && fromCode[p]*10 <= n {
			found = append(found, pkg{a.short(p), n})
		}
	}
	slices.SortFunc(found, func(x, y pkg) int { return cmp.Or(cmp.Compare(y.calls, x.calls), strings.Compare(x.name, y.name)) })
	var out []string
	for _, p := range found[:min(len(found), 3)] {
		out = append(out, fmt.Sprintf("%s (%s)", p.name, plural(p.calls, "call")))
	}
	return out
}

func (a analysis) goErrors() []string {
	errorf, errNew, match := 0, 0, 0
	for full, callers := range a.in.GoCalls {
		switch full {
		case "fmt.Errorf":
			errorf += len(callers)
		case "errors.New":
			errNew += len(callers)
		case "errors.Is", "errors.As":
			match += len(callers)
		}
	}
	if errorf+errNew == 0 {
		return nil
	}
	s := fmt.Sprintf("errors: fmt.Errorf (%s) and errors.New (%d)", plural(errorf, "call"), errNew)
	if match > 0 {
		s += fmt.Sprintf("; errors.Is/As to compare (%d)", match)
	}
	for _, lib := range []string{"github.com/pkg/errors", "github.com/cockroachdb/errors", "github.com/go-faster/errors"} {
		if n := a.goCalls(false, lib); n > 0 {
			s += fmt.Sprintf("; %s (%s)", lib, plural(n, "call"))
		}
	}
	return []string{s}
}

var goLoggers = []string{"log/slog", "go.uber.org/zap", "github.com/rs/zerolog", "github.com/sirupsen/logrus", "github.com/go-logr/logr", "k8s.io/klog", "log"}

// logging names the logger with the most call sites, and the repository's
// own package that wraps it when other packages go through one.
func (a analysis) logging() []string {
	best, most := "", 0
	for _, l := range goLoggers {
		n := 0
		for full, callers := range a.in.GoCalls {
			if goPackage(full) == l {
				n += len(callers)
			}
		}
		if n > most {
			best, most = l, n
		}
	}
	var out []string
	if best != "" {
		s := fmt.Sprintf("logging: %s (%s)", best, plural(most, "call"))
		if w := a.wrapper(best); w != "" {
			s += ", reached through " + w
		}
		out = append(out, s)
	}
	if s := a.imported("logging", false, importLoggers); s != "" {
		out = append(out, s)
	}
	return out
}

// wrapper is an in-repository package with "log" in its name that calls
// logger and that several other packages call.
func (a analysis) wrapper(logger string) string {
	calling := map[string]bool{}
	for full, callers := range a.in.GoCalls {
		if goPackage(full) != logger {
			continue
		}
		for _, c := range callers {
			if p := a.nodes[c].Package; strings.Contains(p[strings.LastIndexByte(p, '/')+1:], "log") {
				calling[p] = true
			}
		}
	}
	users := map[string]map[string]bool{}
	for _, e := range a.in.Graph.Edges() {
		to := a.nodes[e.To]
		if e.Kind != model.EdgeCalls || !calling[to.Package] {
			continue
		}
		if from := a.nodes[e.From].Package; from != to.Package {
			if users[to.Package] == nil {
				users[to.Package] = map[string]bool{}
			}
			users[to.Package][from] = true
		}
	}
	best, most := "", 2
	for p, u := range users {
		if len(u) > most || len(u) == most && p < best {
			best, most = p, len(u)
		}
	}
	if best == "" {
		return ""
	}
	return a.short(best)
}

type lib struct{ label, spec string }

// fileSignal recognises a library by the files that use it, for ecosystems
// whose code does not import it by name (Bundler and Laravel load them;
// Jest provides describe and it as globals).
type fileSignal struct {
	label string
	match func(path string) bool
	unit  string // what is counted, singular
}

var (
	testFiles = []fileSignal{
		{"RSpec", func(p string) bool { return strings.HasSuffix(p, "_spec.rb") }, "spec file"},
		{"Minitest", func(p string) bool { return strings.HasSuffix(p, "_test.rb") }, "test file"},
		{"PHPUnit", func(p string) bool {
			return strings.HasSuffix(p, "Test.php") && strings.Contains("/"+strings.ToLower(p), "/tests/")
		}, "test file"},
		{"Pest", func(p string) bool { return strings.HasSuffix(p, "tests/Pest.php") }, "Pest.php"},
		{"pytest", func(p string) bool { return path.Base(p) == "conftest.py" }, "conftest.py"},
		{"Jest", func(p string) bool { return strings.HasPrefix(path.Base(p), "jest.config.") }, "jest config"},
		{"Vitest", func(p string) bool { return strings.HasPrefix(path.Base(p), "vitest.config.") }, "vitest config"},
	}
	frameworkFiles = []fileSignal{
		{"Rails", func(p string) bool {
			return strings.Contains("/"+p, "/app/controllers/") && strings.HasSuffix(p, "_controller.rb")
		}, "controller"},
		{"Laravel", func(p string) bool {
			return strings.Contains("/"+p, "/app/Http/Controllers/") && strings.HasSuffix(p, ".php")
		}, "controller"},
		{"Symfony", func(p string) bool { return strings.HasSuffix("/"+p, "/config/bundles.php") }, "bundles.php"},
	}
)

// files counts the indexed files a signal matches.
func (a analysis) files(s fileSignal) int {
	n := 0
	for _, f := range a.paths {
		if s.match(f) {
			n++
		}
	}
	return n
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	if strings.HasSuffix(unit, ".php") || strings.HasSuffix(unit, ".py") {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

var (
	importTestLibs = []lib{
		{"pytest", "pytest"}, {"unittest", "unittest"}, {"hypothesis", "hypothesis"},
		{"vitest", "vitest"}, {"jest", "@jest/globals"}, {"mocha", "mocha"}, {"chai", "chai"}, {"node:test", "node:test"},
		{"Testing Library", "@testing-library"}, {"Playwright", "@playwright/test"},
		{"xUnit", "Xunit"}, {"NUnit", "NUnit.Framework"}, {"MSTest", "Microsoft.VisualStudio.TestTools.UnitTesting"},
		{"RSpec", "rspec"}, {"Minitest", "minitest"},
	}
	importLoggers = []lib{
		{"logging", "logging"}, {"structlog", "structlog"}, {"loguru", "loguru"},
		{"pino", "pino"}, {"winston", "winston"}, {"bunyan", "bunyan"},
		{"tracing", "tracing"}, {"Serilog", "Serilog"}, {"Microsoft.Extensions.Logging", "Microsoft.Extensions.Logging"},
	}
	frameworkLibs = []struct {
		area string
		libs []lib
	}{
		{"HTTP", []lib{
			{"gin", "github.com/gin-gonic/gin"}, {"echo", "github.com/labstack/echo"}, {"chi", "github.com/go-chi/chi"},
			{"gorilla/mux", "github.com/gorilla/mux"}, {"fiber", "github.com/gofiber/fiber"}, {"net/http", "net/http"},
			{"FastAPI", "fastapi"}, {"Flask", "flask"}, {"Django", "django"}, {"Starlette", "starlette"},
			{"Express", "express"}, {"Fastify", "fastify"}, {"Hono", "hono"}, {"Koa", "koa"}, {"NestJS", "@nestjs/core"},
			{"axum", "axum"}, {"actix-web", "actix_web"}, {"ASP.NET Core", "Microsoft.AspNetCore"},
		}},
		{"CLI", []lib{
			{"cobra", "github.com/spf13/cobra"}, {"urfave/cli", "github.com/urfave/cli"}, {"kong", "github.com/alecthomas/kong"},
			{"click", "click"}, {"Typer", "typer"}, {"argparse", "argparse"}, {"commander", "commander"}, {"yargs", "yargs"}, {"clap", "clap"},
		}},
		{"database", []lib{
			{"pgx", "github.com/jackc/pgx"}, {"GORM", "gorm.io/gorm"}, {"sqlx", "github.com/jmoiron/sqlx"}, {"database/sql", "database/sql"},
			{"SQLAlchemy", "sqlalchemy"}, {"psycopg", "psycopg"}, {"asyncpg", "asyncpg"},
			{"Prisma", "@prisma/client"}, {"TypeORM", "typeorm"}, {"Drizzle", "drizzle-orm"}, {"Knex", "knex"}, {"Mongoose", "mongoose"},
			{"SQLx", "sqlx"}, {"Diesel", "diesel"}, {"Entity Framework Core", "Microsoft.EntityFrameworkCore"}, {"Dapper", "Dapper"},
		}},
	}
)

// importers counts the importers of spec (or of paths inside it), from
// test code only when tests is set.
func (a analysis) importers(spec string, tests bool) int {
	n := 0
	for imp, ids := range a.in.Imports {
		if !under(imp, spec) && !strings.HasPrefix(imp, spec+".") {
			continue
		}
		for _, id := range ids {
			if !tests || a.nodes[id].IsTest {
				n++
			}
		}
	}
	return n
}

// imported states the libraries of one area that code imports (or whose
// files are there), most used first; "" when none is.
func (a analysis) imported(area string, tests bool, libs []lib, signals ...fileSignal) string {
	type use struct {
		label, unit string
		n           int
	}
	var found []use
	for _, l := range libs {
		if n := a.importers(l.spec, tests); n > 0 {
			found = append(found, use{l.label, "importer", n})
		}
	}
	for _, s := range signals {
		n := a.files(s)
		if n == 0 {
			continue
		}
		// The same library seen both ways: keep the stronger evidence.
		if i := slices.IndexFunc(found, func(u use) bool { return u.label == s.label }); i >= 0 {
			if n > found[i].n {
				found[i] = use{s.label, s.unit, n}
			}
			continue
		}
		found = append(found, use{s.label, s.unit, n})
	}
	if len(found) == 0 {
		return ""
	}
	slices.SortFunc(found, func(x, y use) int { return cmp.Or(cmp.Compare(y.n, x.n), strings.Compare(x.label, y.label)) })
	var parts []string
	for _, f := range found[:min(len(found), 3)] {
		parts = append(parts, fmt.Sprintf("%s (%s)", f.label, plural(f.n, f.unit)))
	}
	return area + ": " + strings.Join(parts, ", ")
}

func (a analysis) importTests() []string {
	if s := a.imported("tests", true, importTestLibs, testFiles...); s != "" {
		return []string{s}
	}
	return nil
}

func (a analysis) frameworks() []string {
	var parts []string
	for _, f := range frameworkLibs {
		var signals []fileSignal
		if f.area == "HTTP" {
			signals = frameworkFiles
		}
		if s := a.imported(f.area, false, f.libs, signals...); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{"frameworks: " + strings.Join(parts, "; ")}
}
