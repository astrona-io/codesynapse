// Command codesynapse indexes repositories into the graph and produces review
// context packs. It is the tool for CI and local use; the API and MCP
// services read what it writes.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/parisnakitakejser/codesynapse/internal/agentcfg"
	"github.com/parisnakitakejser/codesynapse/internal/app"
	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/ingest"
	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/metrics"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/review"
	"github.com/parisnakitakejser/codesynapse/internal/security"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
)

var version = "dev"

func main() {
	os.Exit(app.Main("codesynapse", "Index code, docs and history into a graph for review and AI assistants", version,
		func(ec *app.Env) []*cobra.Command {
			return []*cobra.Command{ingestCmd(ec), reviewCmd(ec), auditCmd(ec), licensesCmd(ec), cleanupCmd(ec), statsCmd(ec), mcpCmd(ec), schemaCmd(ec)}
		}))
}

// ingestOptions configures `codesynapse ingest`.
type ingestOptions struct {
	EC           *app.Env
	neo4j        app.Neo4jFlags
	repo         string
	commitLimit  int
	allowNetwork bool
	vulns        bool
	licenses     bool
	root         string
}

func (o *ingestOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.neo4j.AddFlags(f)
	f.StringVar(&o.repo, "repo", "", "Repository name in the graph (default: directory name)")
	f.IntVar(&o.commitLimit, "commits", 500, "Number of recent commits to index (0 disables history)")
	f.BoolVar(&o.allowNetwork, "allow-network", false, "Allow `go list` to download modules (off by default for untrusted repos)")
	f.BoolVar(&o.vulns, "vulns", false, "Check dependencies against osv.dev (sends module paths and versions)")
	f.BoolVar(&o.licenses, "license-lookup", false, "Look up dependency licences not found on disk on deps.dev (sends package names and versions)")
	return nil
}

func (o *ingestOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	o.root = abs
	if o.repo == "" {
		o.repo = filepath.Base(abs)
	}
	return nil
}

func (o *ingestOptions) Validate(_ context.Context, _ *app.Env) error {
	if o.commitLimit < 0 {
		return errors.New("--commits must be >= 0")
	}
	return nil
}

func (o *ingestOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	g, err := ingest.Run(ctx, ingest.Options{Repo: o.repo, Root: o.root, CommitLimit: o.commitLimit, AllowNetwork: o.allowNetwork, VulnScan: o.vulns, LicenseLookup: o.licenses})
	if err != nil {
		return err
	}
	st, err := o.neo4j.Open(ctx)
	if err != nil {
		return err
	}
	defer st.Close(ctx) //nolint:errcheck // best-effort close
	if err := st.EnsureSchema(ctx); err != nil {
		return err
	}
	if err := st.ReplaceRepo(ctx, o.repo, g); err != nil {
		return err
	}
	o.EC.Logger.Info("graph stored", "repo", o.repo, "nodes", len(g.Nodes()), "edges", len(g.Edges()))
	return nil
}

func ingestCmd(ec *app.Env) *cobra.Command {
	o := &ingestOptions{EC: ec}
	return app.NewSubCommand("ingest [path]", o, ec).
		WithShortDesc("Index a repository into Neo4j").
		WithExample("  CODESYNAPSE_NEO4J_PASSWORD=... codesynapse ingest . --repo my-service").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}

// reviewOptions configures `codesynapse review`.
type reviewOptions struct {
	EC       *app.Env
	neo4j    app.Neo4jFlags
	repo     string
	base     string
	head     string
	diffFile string
	depth    int
	offline  bool
	root     string
}

func (o *reviewOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.neo4j.AddFlags(f)
	f.StringVar(&o.repo, "repo", "", "Repository name in the graph (default: directory name)")
	f.StringVar(&o.base, "base", "origin/main", "Base ref of the change")
	f.StringVar(&o.head, "head", "HEAD", "Head ref of the change")
	f.StringVar(&o.diffFile, "diff", "", "Read a unified diff from this file ('-' for stdin) instead of running git")
	f.IntVar(&o.depth, "depth", 3, "Caller traversal depth")
	f.BoolVar(&o.offline, "offline", false, "Build the graph in memory from the working tree instead of querying Neo4j")
	return nil
}

func (o *reviewOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	o.root = abs
	if o.repo == "" {
		o.repo = filepath.Base(abs)
	}
	return nil
}

func (o *reviewOptions) Validate(_ context.Context, _ *app.Env) error {
	switch o.EC.Output {
	case app.OutputPlain, app.OutputMarkdown, app.OutputJSON:
		return nil
	}
	return fmt.Errorf("unsupported output format %q (plain, markdown, json)", o.EC.Output)
}

func (o *reviewOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	diff, err := o.readDiff(ctx)
	if err != nil {
		return err
	}
	changes, err := gitx.ParseDiff(diff)
	if err != nil {
		return err
	}

	var r store.Reader
	if o.offline {
		g, err := ingest.Run(ctx, ingest.Options{Repo: o.repo, Root: o.root})
		if err != nil {
			return err
		}
		mem := memory.New()
		if err := mem.ReplaceRepo(ctx, o.repo, g); err != nil {
			return err
		}
		r = mem
	} else {
		st, err := o.neo4j.Open(ctx)
		if err != nil {
			return err
		}
		defer st.Close(ctx) //nolint:errcheck // best-effort close
		r = st
	}

	rep, err := review.Analyze(ctx, query.Service{R: r}, o.repo, changes, review.Options{Depth: o.depth})
	if err != nil {
		return err
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return review.WriteMarkdown(o.EC.Stdout, rep)
}

func (o *reviewOptions) readDiff(ctx context.Context) ([]byte, error) {
	switch o.diffFile {
	case "":
		return gitx.Diff(ctx, o.root, o.base, o.head)
	case "-":
		return io.ReadAll(io.LimitReader(o.EC.Stdin, 64<<20))
	default:
		return os.ReadFile(o.diffFile)
	}
}

func reviewCmd(ec *app.Env) *cobra.Command {
	o := &reviewOptions{EC: ec}
	return app.NewSubCommand("review [path]", o, ec).
		WithShortDesc("Produce a review context pack for a change").
		WithLongDesc("Maps a diff onto the graph: changed symbols with risk, impacted callers, covering tests, docs to update and a security checklist. Output is Markdown (default) or JSON (-o json).").
		WithExample("  codesynapse review . --base origin/main\n  git diff | codesynapse review --diff - --offline\n  codesynapse review --offline -o json > review.json").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}

// schemaOptions configures `codesynapse schema`.
type schemaOptions struct {
	EC    *app.Env
	neo4j app.Neo4jFlags
}

func (o *schemaOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.neo4j.AddFlags(o.EC.Command.Flags())
	return nil
}
func (o *schemaOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *schemaOptions) Validate(context.Context, *app.Env) error { return nil }
func (o *schemaOptions) Run(ctx context.Context, _ *app.Env) error {
	st, err := o.neo4j.Open(ctx)
	if err != nil {
		return err
	}
	defer st.Close(ctx) //nolint:errcheck // best-effort close
	return st.EnsureSchema(ctx)
}

func schemaCmd(ec *app.Env) *cobra.Command {
	o := &schemaOptions{EC: ec}
	return app.NewSubCommand("schema", o, ec).
		WithShortDesc("Create Neo4j constraints and indexes").
		WithGroupID(app.GroupOther).
		WithNoArgs().
		Build()
}

// statsOptions configures `codesynapse stats`.
type statsOptions struct {
	EC      *app.Env
	samples int
	root    string
}

func (o *statsOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().IntVar(&o.samples, "samples", 150, "Functions sampled for the token benchmark")
	return nil
}

func (o *statsOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *statsOptions) Validate(context.Context, *app.Env) error { return nil }

func (o *statsOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	repo := filepath.Base(o.root)
	g, err := ingest.Run(ctx, ingest.Options{Repo: repo, Root: o.root, CommitLimit: 500})
	if err != nil {
		return err
	}
	rep, err := metrics.Compute(ctx, repo, g, metrics.Options{BenchmarkSamples: o.samples})
	if err != nil {
		return err
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return writeStats(o.EC.Stdout, repo, rep)
}

func writeStats(w io.Writer, repo string, r metrics.Report) error {
	c, t, d, b := r.Code, r.Tests, r.Docs, r.Benchmark
	cx := r.Distributions["complexity"]
	_, err := fmt.Fprintf(w, `%s
  code        %d packages · %d files · %d LOC (+%d test LOC) · %d functions · %d methods · %d types
  complexity  avg %.1f · p50 %d · p90 %d · p99 %d · max %d · %d over 10 · %d over 20
  tests       %d test funcs · test/code LOC %.2f · %d/%d functions reached by a test (%.1f%%) · %d exported untested
  docs        %.1f%% of %d exported symbols documented · %d docs · %d sections · %d symbols mentioned
  security    %d functions call sensitive APIs
  dead code   %d candidates (unexported, no static callers)
  benchmark   tokens reduced by %.1f%% (median per question, %.1f%% overall) · %d samples · graph %d vs baseline %d tokens
              (%s)
`, repo,
		c.Packages, c.Files, c.LOC, c.TestLOC, c.Functions, c.Methods, c.Types,
		c.AvgComplexity, cx.P50, cx.P90, cx.P99, cx.Max, c.ComplexOver10, c.ComplexOver20,
		t.TestFunctions, t.TestToCodeRatio, t.Reachable, t.Total, t.ReachablePct, t.ExportedUntested,
		d.CoveragePct, d.Exported, d.Documents, d.Sections, d.SymbolsMentioned,
		c.SecuritySymbols, len(r.DeadCode),
		b.MedianReductionPct, b.TotalReductionPct, b.Samples, b.TotalGraph, b.TotalBaseline, b.Method)
	return err
}

func statsCmd(ec *app.Env) *cobra.Command {
	o := &statsOptions{EC: ec}
	return app.NewSubCommand("stats [path]", o, ec).
		WithShortDesc("Compute repository metrics and the token benchmark (offline)").
		WithExample("  codesynapse stats .\n  codesynapse stats . -o json > metrics.json").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}

// auditOptions configures `codesynapse audit`.
type auditOptions struct {
	EC          *app.Env
	vulns       bool
	minSeverity string
	failOn      string
	root        string
}

// errThreshold makes the process exit non-zero for CI without printing an
// error banner on top of the report.
var errThreshold = errors.New("security findings at or above the --fail-on threshold")

func (o *auditOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.BoolVar(&o.vulns, "vulns", true, "Check dependencies against osv.dev (sends module paths and versions)")
	f.StringVar(&o.minSeverity, "min-severity", "low", "Hide findings below this severity (info, low, medium, high, critical)")
	f.StringVar(&o.failOn, "fail-on", "", "Exit 1 if a finding or reachable vulnerability is at or above this severity")
	return nil
}

func (o *auditOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *auditOptions) Validate(context.Context, *app.Env) error {
	for _, s := range []string{o.minSeverity, o.failOn} {
		if s != "" && model.SeverityRank(s) < 0 {
			return fmt.Errorf("unknown severity %q", s)
		}
	}
	return nil
}

func (o *auditOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	repo := filepath.Base(o.root)
	g, err := ingest.Run(ctx, ingest.Options{Repo: repo, Root: o.root, VulnScan: o.vulns})
	if err != nil {
		return err
	}
	mem := memory.New()
	if err := mem.ReplaceRepo(ctx, repo, g); err != nil {
		return err
	}
	rep, err := security.Build(ctx, mem, repo, security.Options{MinSeverity: o.minSeverity})
	if err != nil {
		return err
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(rep)
	} else {
		err = security.WriteMarkdown(o.EC.Stdout, rep)
	}
	if err != nil {
		return err
	}
	if o.failOn != "" && rep.ExceedsThreshold(o.failOn) {
		return errThreshold
	}
	return nil
}

func auditCmd(ec *app.Env) *cobra.Command {
	o := &auditOptions{EC: ec}
	return app.NewSubCommand("audit [path]", o, ec).
		WithShortDesc("Security review: code findings and vulnerable dependencies (offline graph)").
		WithLongDesc("Runs the security rules over the type-checked code and checks go.mod dependencies and the Go standard library against osv.dev. Findings are ranked by severity, exposure from entry points and test reachability; vulnerabilities by severity and whether the vulnerable symbol is called.").
		WithExample("  codesynapse audit .\n  codesynapse audit . --fail-on high      # CI gate\n  codesynapse audit . --vulns=false -o json").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}

// licensesOptions configures `codesynapse licenses`.
type licensesOptions struct {
	EC     *app.Env
	lookup bool
	failOn string
	root   string
}

// errLicenses makes `licenses --fail-on` exit non-zero without an error banner.
var errLicenses = errors.New("dependency licences at or above the --fail-on level")

func (o *licensesOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.BoolVar(&o.lookup, "license-lookup", false, "Look up licences not found on disk on deps.dev (sends package names and versions)")
	f.StringVar(&o.failOn, "fail-on", "", "Exit 1 on any dependency with this verdict or worse: conflict or review")
	return nil
}

func (o *licensesOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *licensesOptions) Validate(context.Context, *app.Env) error {
	if o.failOn != "" && o.failOn != license.Conflict && o.failOn != license.Review {
		return fmt.Errorf("--fail-on must be %q or %q", license.Conflict, license.Review)
	}
	return nil
}

func (o *licensesOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	rep, err := ingest.Licenses(ctx, o.root, ingest.Options{LicenseLookup: o.lookup})
	if err != nil {
		return err
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(rep)
	} else {
		_, err = io.WriteString(o.EC.Stdout, license.Markdown(filepath.Base(o.root), rep))
	}
	if err != nil {
		return err
	}
	if rep.Conflict > 0 && o.failOn != "" || rep.Review > 0 && o.failOn == license.Review {
		return errLicenses
	}
	return nil
}

func licensesCmd(ec *app.Env) *cobra.Command {
	o := &licensesOptions{EC: ec}
	return app.NewSubCommand("licenses [path]", o, ec).
		WithShortDesc("Check dependency licences against the repository's own licence").
		WithLongDesc("Reads go.mod and npm/PyPI lockfiles, finds each dependency's licence (lockfile, vendor, Go module cache, node_modules, site-packages; deps.dev with --license-lookup) and explains whether it fits the project's licence. Guidance, not legal advice: every verdict says why and where the licence was read.").
		WithExample("  codesynapse licenses .\n  codesynapse licenses . --license-lookup --fail-on conflict   # CI gate\n  codesynapse licenses . -o json").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}

// mcpInstallOptions configures `codesynapse mcp install`.
type mcpInstallOptions struct {
	EC   *app.Env
	url  string
	yes  bool
	only []string
}

func (o *mcpInstallOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.url, "url", app.Getenv("CODESYNAPSE_MCP_URL", "http://localhost:8090/mcp"), "MCP endpoint to register")
	f.BoolVarP(&o.yes, "yes", "y", false, "Install into every detected agent without asking")
	f.StringSliceVar(&o.only, "only", nil, "Limit to these agents (claude, codex, gemini, opencode, cursor, project)")
	return nil
}
func (o *mcpInstallOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *mcpInstallOptions) Validate(context.Context, *app.Env) error {
	if !o.yes && !o.EC.IsTerminal {
		return errors.New("not a terminal: pass --yes (and --only) to install without prompts")
	}
	return nil
}

func (o *mcpInstallOptions) Run(ctx context.Context, _ *app.Env) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	server := agentcfg.Server{URL: o.url, Token: os.Getenv(agentcfg.TokenEnv)}
	in := bufio.NewReader(o.EC.Stdin)
	installed := 0
	for _, t := range agentcfg.Detect(home, cwd) {
		if len(o.only) > 0 && !slices.Contains(o.only, t.ID) {
			continue
		}
		if !o.yes && !confirm(o.EC.Stderr, in, t) {
			continue
		}
		if err := t.Install(ctx, server); err != nil {
			_, _ = fmt.Fprintf(o.EC.Stderr, "  ✗ %s: %v\n", t.Description, err)
			continue
		}
		installed++
		_, _ = fmt.Fprintf(o.EC.Stderr, "  ✓ %s\n", t.Description)
	}
	if installed > 0 {
		_, _ = fmt.Fprintf(o.EC.Stderr, "\nAgents other than Claude read the token from $%s. Export it in your shell profile, e.g.\n  export %s=$(grep ^%s= %s | cut -d= -f2)\n",
			agentcfg.TokenEnv, agentcfg.TokenEnv, agentcfg.TokenEnv, filepath.Join(cwd, ".env"))
	}
	return nil
}

func confirm(w io.Writer, in *bufio.Reader, t agentcfg.Target) bool {
	note := ""
	if t.StoresSecret {
		note = " [stores the token in its config]"
	}
	_, _ = fmt.Fprintf(w, "Add CodeSynapse to %s%s? [Y/n] ", t.Description, note)
	line, _ := in.ReadString('\n') //nolint:errcheck // EOF means no
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}

func mcpCmd(ec *app.Env) *cobra.Command {
	parent := &cobra.Command{Use: "mcp", Short: "Connect AI agents to the CodeSynapse MCP server", GroupID: app.GroupBase}
	o := &mcpInstallOptions{EC: ec}
	parent.AddCommand(app.NewSubCommand("install", o, ec).
		WithShortDesc("Register the MCP server with Claude Code, Codex, Gemini, OpenCode, Cursor or a project .mcp.json").
		WithExample("  codesynapse mcp install\n  codesynapse mcp install --yes --only claude,codex").
		WithNoArgs().
		Build())
	return parent
}

// cleanupOptions configures `codesynapse cleanup`.
type cleanupOptions struct {
	EC    *app.Env
	vulns bool
	limit int
	root  string
}

func (o *cleanupOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.BoolVar(&o.vulns, "vulns", true, "Check dependencies against osv.dev, to show CVEs removable with unused modules")
	f.IntVar(&o.limit, "limit", 100, "Max functions listed in Markdown output (0 = all)")
	return nil
}

func (o *cleanupOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *cleanupOptions) Validate(context.Context, *app.Env) error { return nil }

func (o *cleanupOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	repo := filepath.Base(o.root)
	g, err := ingest.Run(ctx, ingest.Options{Repo: repo, Root: o.root, VulnScan: o.vulns})
	if err != nil {
		return err
	}
	n, ok := g.Node(model.NodeID(repo, model.KindRepository, repo))
	if !ok || n.Cleanup == "" {
		return errors.New("cleanup analysis did not run")
	}
	var rep cleanup.Report
	if err := json.Unmarshal([]byte(n.Cleanup), &rep); err != nil {
		return err
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return cleanup.WriteMarkdown(o.EC.Stdout, repo, rep, o.limit)
}

func cleanupCmd(ec *app.Env) *cobra.Command {
	o := &cleanupOptions{EC: ec}
	return app.NewSubCommand("cleanup [path]", o, ec).
		WithShortDesc("Find unused code, dependencies and config to remove (offline graph)").
		WithLongDesc("Reachability from entry points over calls, interface dispatch and function references: unreachable and test-only functions, files with nothing live, unused go.mod requirements (with the CVEs removing them eliminates), unused Helm values and template defines. Security-relevant dead code is listed first.").
		WithExample("  codesynapse cleanup .\n  codesynapse cleanup . -o json").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}
