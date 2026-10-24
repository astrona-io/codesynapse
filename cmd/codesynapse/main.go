// Command codesynapse indexes repositories into the graph and produces review
// context packs. It is the tool for CI and local use; the API and MCP
// services read what it writes.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/cleanup"
	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/metrics"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/sbom"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/snapshot"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/toolsets"
)

var version = "dev"

func main() {
	os.Exit(app.Main("codesynapse", "Index code, docs and history into a graph for review and AI assistants", version,
		commands))
}

// commands are the CLI's subcommands (the plugin test checks the hooks
// call ones that exist).
func commands(ec *app.Env) []*cobra.Command {
	return []*cobra.Command{ingestCmd(ec), reviewCmd(ec), auditCmd(ec), licensesCmd(ec), cleanupCmd(ec), duplicatesCmd(ec), sbomCmd(ec), upgradeCmd(ec), statsCmd(ec), evalCmd(ec), hookCmd(ec), gitHookCmd(ec), installCmd(ec), statusCmd(ec), uninstallCmd(ec), mcpCmd(ec), memoryCmd(ec), runtimeCmd(ec), schemaCmd(ec), repoConfigCmd(ec)}
}

// ingestOptions configures `codesynapse ingest`.
type ingestOptions struct {
	approvedPolicyDigest string
	reportFile           string
	EC                   *app.Env
	graph                app.GraphFlags
	repo                 string
	nameFromOrigin       bool
	commitLimit          int
	allowNetwork         bool
	vulns                bool
	licenses             bool
	dataflow             bool
	root                 string
	watch                bool
	interval             time.Duration
	st                   store.Store // opened by the first run, kept by --watch
}

func (o *ingestOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
	f.StringVar(&o.reportFile, "ingest-report", "", "Write the per-file JSON report to a new file (existing files are refused)")
	o.graph.AddFlags(f)
	f.StringVar(&o.repo, "repo", "", "Repository name in the graph (default: directory name)")
	f.BoolVar(&o.nameFromOrigin, "name-from-origin", false, "Name the repository after the checkout's origin remote the way the dashboard names one added by URL (owner/name), so adding that URL there links this index instead of cloning it")
	f.IntVar(&o.commitLimit, "commits", 500, "Number of recent commits to index (0 disables history)")
	f.BoolVar(&o.allowNetwork, "allow-network", false, "Allow `go list` to download modules (off by default for untrusted repos)")
	f.BoolVar(&o.vulns, "vulns", false, "Check dependencies against osv.dev (sends module paths and versions)")
	f.BoolVar(&o.licenses, "license-lookup", false, "Look up dependency licences not found on disk on deps.dev (sends package names and versions)")
	f.BoolVar(&o.dataflow, "dataflow", false, "Resolve Go calls through function values with SSA and VTA (slower; edges marked dataflow)")
	f.BoolVar(&o.watch, "watch", false, "Keep running and re-index when the git working tree changes (edits, commits, checkouts)")
	f.DurationVar(&o.interval, "watch-interval", 2*time.Second, "How often --watch looks at the working tree; a change is indexed once it held for one interval")
	return nil
}

func (o *ingestOptions) Complete(ctx context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	if err != nil {
		return err
	}
	o.root = abs
	if o.nameFromOrigin && o.repo == "" {
		name := forge.NameFromRemote(gitx.SanitizeURL(gitx.HTTPSRemote(gitx.OriginURL(ctx, abs))))
		if name == "" {
			return errors.New("--name-from-origin needs an origin remote with an https or ssh address of the form host/owner/name; name the repository with --repo instead")
		}
		o.repo = name
	}
	if o.repo == "" {
		o.repo = filepath.Base(abs)
	}
	return nil
}

func (o *ingestOptions) Validate(_ context.Context, _ *app.Env) error {
	if o.commitLimit < 0 {
		return errors.New("--commits must be >= 0")
	}
	if o.watch && o.reportFile != "" {
		return errors.New("--ingest-report writes one run's report; leave it out with --watch")
	}
	if o.watch && o.interval < 500*time.Millisecond {
		return errors.New("--watch-interval must be at least 500ms")
	}
	return nil
}

func (o *ingestOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	defer func() {
		if o.st != nil {
			o.st.Close(ctx) //nolint:errcheck,gosec // best-effort close
		}
	}()
	if !o.watch {
		return o.once(ctx)
	}
	return o.watchLoop(ctx)
}

// openStore connects on first use and keeps the store for later runs.
func (o *ingestOptions) openStore(ctx context.Context) (store.Store, error) {
	if o.st != nil {
		return o.st, nil
	}
	st, err := o.graph.Open(ctx)
	if err != nil {
		return nil, err
	}
	if err := st.EnsureSchema(ctx); err != nil {
		st.Close(ctx) //nolint:errcheck,gosec // the schema error is the one to report
		return nil, err
	}
	o.st = st
	return st, nil
}

// once indexes the repository and publishes it, with its accounting.
func (o *ingestOptions) once(ctx context.Context) (result error) {
	var runID [16]byte
	if _, err := rand.Read(runID[:]); err != nil {
		return err
	}
	ledger, err := ingestreport.New(hex.EncodeToString(runID[:]), time.Now().UTC())
	if err != nil {
		return err
	}
	var output *os.File
	if o.reportFile != "" {
		output, err = os.OpenFile(o.reportFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create ingest report: %w", err)
		}
	}
	commit := ingestreport.CommitNone
	defer func() {
		status := ingestreport.Failed
		if commit == ingestreport.CommitCommitted {
			status = ingestreport.Succeeded
		} else if errors.Is(result, context.Canceled) || errors.Is(result, context.DeadlineExceeded) {
			status = ingestreport.Cancelled
		}
		report, err := ledger.Snapshot().Finish(status, commit)
		if err != nil {
			result = errors.Join(result, err)
		} else {
			o.EC.Logger.Info("ingest accounting completed", "run", report.RunID, "repo", o.repo,
				"scanned", report.TotalFilesScanned, "indexed", report.FilesIndexed,
				"skipped", report.FilesSkipped, "failed", report.FilesFailed,
				"unprocessed", report.FilesUnprocessed, "graph_commit", report.GraphCommit,
				"discovery_complete", report.DiscoveryComplete)
			if output != nil {
				enc := json.NewEncoder(output)
				enc.SetIndent("", "  ")
				result = errors.Join(result, enc.Encode(report))
			}
		}
		if output != nil {
			result = errors.Join(result, output.Close())
		}
	}()
	ctx = ingestreport.WithLedger(ctx, ledger)
	st, err := o.openStore(ctx)
	if err != nil {
		return err
	}
	previous, err := ingest.IdentityBaseline(ctx, st, o.repo)
	if err != nil {
		return err
	}
	g, err := ingest.Run(ctx, ingest.Options{Repo: o.repo, Root: o.root, CommitLimit: o.commitLimit, AllowNetwork: o.allowNetwork, VulnScan: o.vulns, LicenseLookup: o.licenses, Dataflow: o.dataflow, PreviousIdentities: previous, ApprovedPolicyDigest: o.approvedPolicyDigest})
	if err != nil {
		return err
	}
	ledger.Prepare(g)
	commit = ingestreport.CommitUnknown
	if err := st.ReplaceRepo(ctx, o.repo, g); err != nil {
		return err
	}
	commit = ingestreport.CommitCommitted
	snapshot.Record(ctx, st, o.repo)
	o.EC.Logger.Info("graph stored", "repo", o.repo, "nodes", len(g.Nodes()), "edges", len(g.Edges()))
	return nil
}

func ingestCmd(ec *app.Env) *cobra.Command {
	o := &ingestOptions{EC: ec}
	return app.NewSubCommand("ingest [path]", o, ec).
		WithShortDesc("Index a repository into HugeGraph").
		WithLongDesc("Indexes the repository at path and publishes its graph; publication writes only what changed since the last run. With --watch it keeps running and re-indexes whenever the git working tree changes, so the graph follows your edits; `codesynapse git-hook install` re-indexes after each commit instead.").
		WithExample("  CODESYNAPSE_HUGEGRAPH_PASSWORD=... codesynapse ingest . --repo my-service\n  codesynapse ingest . --repo my-service --watch").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}

// reviewOptions configures `codesynapse review`.
type reviewOptions struct {
	approvedPolicyDigest string
	EC                   *app.Env
	graph                app.GraphFlags
	repo                 string
	base                 string
	head                 string
	diffFile             string
	depth                int
	offline              bool
	findingsDelta        bool
	coverage             []string
	junit                []string
	failOnNew            string
	root                 string
}

func (o *reviewOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
	f := o.EC.Command.Flags()
	o.graph.AddFlags(f)
	f.StringVar(&o.repo, "repo", "", "Repository name in the graph (default: directory name)")
	f.StringVar(&o.base, "base", "origin/main", "Base ref of the change")
	f.StringVar(&o.head, "head", "HEAD", "Head ref of the change")
	f.StringVar(&o.diffFile, "diff", "", "Read a unified diff from this file ('-' for stdin) instead of running git")
	f.IntVar(&o.depth, "depth", 3, "Caller traversal depth")
	f.BoolVar(&o.offline, "offline", false, "Build the graph in memory from the working tree instead of querying HugeGraph")
	f.BoolVar(&o.findingsDelta, "findings-delta", true, "With --offline and a git diff, also index the base and report which security findings the change introduces and fixes")
	f.StringSliceVar(&o.coverage, "coverage", nil, "With --offline, a Go coverage profile or LCOV file of the last test run (default: cover.out, lcov.info and the like at the root)")
	f.StringVar(&o.failOnNew, "fail-on-new", "", "With --offline and a git diff, exit 1 when the change introduces a security finding at or above this severity (pre-existing findings do not count)")
	f.StringSliceVar(&o.junit, "junit", nil, "With --offline, a JUnit XML report of the last test run (default: junit.xml, report.xml and the like at the root)")
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
	if (len(o.coverage) > 0 || len(o.junit) > 0) && !o.offline {
		return errors.New("--coverage and --junit need --offline: they are read with the working tree")
	}
	if o.failOnNew != "" {
		if model.SeverityRank(o.failOnNew) < 0 {
			return fmt.Errorf("--fail-on-new must be one of %s", strings.Join(model.Severities, ", "))
		}
		if !o.offline || !o.findingsDelta || o.diffFile != "" {
			return errors.New("--fail-on-new needs --offline with a git base (not --diff): the base is indexed to tell new findings from old")
		}
	}
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
		g, err := ingest.Run(ctx, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: o.repo, Root: o.root})
		if err != nil {
			return err
		}
		mem := memory.New()
		if err := mem.ReplaceRepo(ctx, o.repo, g); err != nil {
			return err
		}
		if err := o.loadRuntimeFacts(ctx, mem, g); err != nil {
			return err
		}
		r = mem
	} else {
		st, err := o.graph.Open(ctx)
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
	if o.offline && o.findingsDelta && o.diffFile == "" {
		if rep.Security, err = o.securityDelta(ctx, r, changes); err != nil {
			return err
		}
	}
	if f, ok, err := store.RuntimeFactsOf(ctx, r, o.repo); err != nil {
		return err
	} else if ok {
		rep.Measured = review.MeasuredOf(f, false, rep)
	}
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else if err := review.WriteMarkdown(o.EC.Stdout, rep); err != nil {
		return err
	}
	if o.failOnNew != "" && rep.Security != nil && rep.Security.IntroducesAtLeast(o.failOnNew) {
		return errNewFindings
	}
	return nil
}

// errNewFindings makes `review --fail-on-new` exit non-zero for CI without
// printing an error banner on top of the report.
var errNewFindings = errors.New("the change introduces security findings at or above the --fail-on-new threshold")

// loadRuntimeFacts reads the coverage and JUnit reports named by flags, or
// found at the root, and keeps what they measured next to the graph.
func (o *reviewOptions) loadRuntimeFacts(ctx context.Context, mem *memory.Store, g *model.Graph) error {
	files := runtimefacts.Files{Coverage: o.coverage, JUnit: o.junit}
	if files.Empty() {
		files = runtimefacts.Discover(o.root)
	}
	if files.Empty() {
		return nil
	}
	f, err := runtimefacts.Load(ctx, runtimefacts.NodeSource(g.Nodes()), files)
	if err != nil {
		return fmt.Errorf("reading test reports: %w", err)
	}
	return mem.SetRuntimeFacts(ctx, o.repo, f)
}

// securityDelta indexes where the change left the base branch and compares
// the findings of the changed files with the working tree's.
func (o *reviewOptions) securityDelta(ctx context.Context, head store.Reader, changes []gitx.FileChange) (*review.SecurityDelta, error) {
	for _, rev := range []string{o.base, o.head} {
		if err := gitx.ValidateRev(rev); err != nil {
			return nil, err
		}
	}
	out, err := gitx.Run(ctx, o.root, "merge-base", "--end-of-options", o.base, o.head)
	if err != nil {
		return nil, fmt.Errorf("finding where the change left %s: %w", o.base, err)
	}
	commit := strings.TrimSpace(string(out))
	g, err := ingest.AtCommit(ctx, o.root, commit, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: o.repo})
	if err != nil {
		return nil, fmt.Errorf("indexing the base %s: %w", short(commit), err)
	}
	base := memory.New()
	if err := base.ReplaceRepo(ctx, o.repo, g); err != nil {
		return nil, err
	}
	files := make([]string, 0, len(changes))
	for _, c := range changes {
		files = append(files, c.Path)
	}
	d, err := review.CompareFindings(ctx, base, head, o.repo, files)
	if err != nil {
		return nil, err
	}
	d.Base = o.base + " (" + short(commit) + ")"
	return &d, nil
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
	graph app.GraphFlags
}

func (o *schemaOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.graph.AddFlags(o.EC.Command.Flags())
	return nil
}
func (o *schemaOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *schemaOptions) Validate(context.Context, *app.Env) error { return nil }
func (o *schemaOptions) Run(ctx context.Context, _ *app.Env) error {
	st, err := o.graph.Open(ctx)
	if err != nil {
		return err
	}
	defer st.Close(ctx) //nolint:errcheck // best-effort close
	return st.EnsureSchema(ctx)
}

func schemaCmd(ec *app.Env) *cobra.Command {
	o := &schemaOptions{EC: ec}
	return app.NewSubCommand("schema", o, ec).
		WithShortDesc("Create the HugeGraph schema (property keys, labels, indexes)").
		WithGroupID(app.GroupOther).
		WithNoArgs().
		Build()
}

// statsOptions configures `codesynapse stats`.
type statsOptions struct {
	approvedPolicyDigest string
	EC                   *app.Env
	samples              int
	root                 string
}

func (o *statsOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
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
	g, err := ingest.Run(ctx, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: repo, Root: o.root, CommitLimit: 500})
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
	approvedPolicyDigest string
	EC                   *app.Env
	vulns                bool
	minSeverity          string
	failOn               string
	root                 string
}

// errThreshold makes the process exit non-zero for CI without printing an
// error banner on top of the report.
var errThreshold = errors.New("security findings at or above the --fail-on threshold")

func (o *auditOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
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
	g, err := ingest.Run(ctx, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: repo, Root: o.root, VulnScan: o.vulns})
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
	approvedPolicyDigest string
	EC                   *app.Env
	lookup               bool
	failOn               string
	root                 string
}

// errLicenses makes `licenses --fail-on` exit non-zero without an error banner.
var errLicenses = errors.New("dependency licences at or above the --fail-on level")

func (o *licensesOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
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
	rep, err := ingest.Licenses(ctx, o.root, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, LicenseLookup: o.lookup})
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
	profile string
	EC      *app.Env
	url     string
	apiURL  string
	yes     bool
	only    []string
}

func (o *mcpInstallOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	f.StringVar(&o.url, "url", app.Getenv("CODESYNAPSE_MCP_URL", "http://localhost:8090/mcp"), "MCP endpoint to register")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "API the Claude Code bridge registers chat sessions with")
	f.BoolVarP(&o.yes, "yes", "y", false, "Install into every detected agent without asking")
	f.StringSliceVar(&o.only, "only", nil, "Limit to these agents (claude, codex, gemini, opencode, cursor, project)")
	f.StringVar(&o.profile, "profile", toolsets.Core, "Toolsets the agents get: core (the eleven everyday tools), all, or core plus a comma list of "+strings.Join(toolsets.Names(), ", "))
	return nil
}
func (o *mcpInstallOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *mcpInstallOptions) Validate(context.Context, *app.Env) error {
	if !o.yes && !o.EC.IsTerminal {
		return errors.New("not a terminal: pass --yes (and --only) to install without prompts")
	}
	sel, err := toolsets.Parse(o.profile)
	if err != nil {
		return fmt.Errorf("--profile: %w", err)
	}
	o.url, err = withToolsets(o.url, sel)
	return err
}

// withToolsets adds the selection to the MCP URL (?toolsets=core,memory);
// nil (all) leaves it as is, so the server's default applies.
func withToolsets(raw string, sel []string) (string, error) {
	if sel == nil {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("--url: %w", err)
	}
	q := u.Query()
	q.Set("toolsets", strings.Join(sel, ","))
	u.RawQuery = q.Encode()
	return u.String(), nil
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
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	if strings.Contains(bin, "go-build") {
		// `go run` builds into a temporary folder the bridge would vanish from.
		return errors.New("run an installed codesynapse binary (go install ./cmd/codesynapse, or just mcp), not go run")
	}
	bin = brewStablePath(bin)
	server := agentcfg.Server{URL: o.url, Token: os.Getenv(agentcfg.TokenEnv), Command: bin, APIURL: o.apiURL, StoreToken: storeToken}
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

// brewStablePath swaps a Homebrew Cellar path, which `brew upgrade` removes,
// for the opt symlink that follows upgrades.
func brewStablePath(bin string) string {
	prefix, rest, ok := strings.Cut(bin, "/Cellar/codesynapse/")
	if !ok {
		return bin
	}
	if _, file, ok := strings.Cut(rest, "/"); ok {
		return prefix + "/opt/codesynapse/" + file
	}
	return bin
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
	parent.AddCommand(app.NewSubCommand("bridge", &mcpBridgeOptions{EC: ec}, ec).
		WithShortDesc("Stdio MCP server for one chat: registers the chat's session and forwards to the HTTP server").
		WithLongDesc("Claude Code starts this per chat (`codesynapse mcp install` configures it). It reads the chat ID from CLAUDE_CODE_SESSION_ID (or CODESYNAPSE_SESSION_ID), registers it through the API with the key from CODESYNAPSE_API_TOKEN or the file `mcp install` wrote, and forwards every MCP message with the key and the runtime session headers.").
		WithNoArgs().
		Build())
	parent.AddCommand(app.NewSubCommand("agents-md [dir]", &mcpAgentsMdOptions{EC: ec}, ec).
		WithShortDesc("Add the CodeSynapse memory block to a repository's AGENTS.md (and CLAUDE.md when it does not import AGENTS.md)").
		WithExample("  codesynapse mcp agents-md ~/src/api").
		WithMaxArgs(1).
		Build())
	parent.AddCommand(app.NewSubCommand("hooks [dir]", &mcpHooksOptions{EC: ec}, ec).
		WithShortDesc("Install Claude Code hooks: recall memory at session start, offload older turns before compaction").
		WithExample("  export CODESYNAPSE_API_TOKEN=...   # from the CodeSynapse .env\n  codesynapse mcp hooks ~/src/api\n  codesynapse mcp hooks . --shared --repo astrona-io/api").
		WithMaxArgs(1).
		Build())
	return parent
}

// cleanupOptions configures `codesynapse cleanup`.
type cleanupOptions struct {
	approvedPolicyDigest string
	EC                   *app.Env
	vulns                bool
	limit                int
	root                 string
}

func (o *cleanupOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
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
	g, err := ingest.Run(ctx, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: repo, Root: o.root, VulnScan: o.vulns})
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

// duplicatesOptions configures `codesynapse duplicates`.
type duplicatesOptions struct {
	approvedPolicyDigest string
	EC                   *app.Env
	limit                int
	maxPercent           float64
	roots                []string
}

func (o *duplicatesOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
	f := o.EC.Command.Flags()
	f.IntVar(&o.limit, "limit", 20, "Max groups listed in Markdown output (0 = all)")
	f.Float64Var(&o.maxPercent, "max-percent", 0, "Exit with an error when a repository repeats more than this share of its function lines, exact and edited copies together (0 = never fail)")
	return nil
}

func (o *duplicatesOptions) Complete(_ context.Context, _ *app.Env) error {
	args := o.EC.Args
	if len(args) == 0 {
		args = []string{"."}
	}
	for _, a := range args {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		o.roots = append(o.roots, abs)
	}
	return nil
}

func (o *duplicatesOptions) Validate(context.Context, *app.Env) error {
	if o.maxPercent < 0 || o.maxPercent > 100 {
		return fmt.Errorf("--max-percent must be between 0 and 100, got %v", o.maxPercent)
	}
	return nil
}

func (o *duplicatesOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	reports := map[string]duplicates.Report{}
	var names []string
	for _, root := range o.roots {
		repo := filepath.Base(root)
		g, err := ingest.Run(ctx, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: repo, Root: root})
		if err != nil {
			return fmt.Errorf("%s: %w", root, err)
		}
		n, ok := g.Node(model.NodeID(repo, model.KindRepository, repo))
		if !ok || n.Duplicates == "" {
			return fmt.Errorf("%s: duplicate analysis did not run", root)
		}
		rep, err := duplicates.Decode(n.Duplicates)
		if err != nil {
			return err
		}
		reports[repo] = rep
		names = append(names, repo)
	}
	var out any
	if len(names) == 1 {
		rep := reports[names[0]]
		rep.Fingerprints = nil
		out = rep
	} else {
		out = duplicates.Across(reports)
	}
	if err := o.write(names, out); err != nil {
		return err
	}
	return o.gate(names, reports)
}

func (o *duplicatesOptions) write(names []string, out any) error {
	if o.EC.Output == app.OutputJSON {
		enc := json.NewEncoder(o.EC.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	if cross, ok := out.(duplicates.CrossReport); ok {
		return duplicates.WriteCrossMarkdown(o.EC.Stdout, cross, o.limit)
	}
	return duplicates.WriteMarkdown(o.EC.Stdout, names[0], out.(duplicates.Report), o.limit)
}

// gate fails a CI run when a repository repeats more than --max-percent of
// its function lines. The report has already been printed.
func (o *duplicatesOptions) gate(names []string, reports map[string]duplicates.Report) error {
	if o.maxPercent <= 0 {
		return nil
	}
	var over []string
	for _, name := range names {
		if p := reports[name].Summary.Percent; p > o.maxPercent {
			over = append(over, fmt.Sprintf("%s repeats %.1f%% of its function lines", name, p))
		}
	}
	if len(over) > 0 {
		return fmt.Errorf("duplicate code above %.1f%%: %s", o.maxPercent, strings.Join(over, "; "))
	}
	return nil
}

func duplicatesCmd(ec *app.Env) *cobra.Command {
	o := &duplicatesOptions{EC: ec}
	return app.NewSubCommand("duplicates [path...]", o, ec).
		WithShortDesc("Find code that exists more than once, in one repository or across several (offline graph)").
		WithLongDesc("Groups functions and methods with the same structure (names and literal values ignored) and ranks them by redundant lines. With several paths it also finds code copied between the repositories and recommends where a shared library or framework would pay off.").
		WithExample("  codesynapse duplicates .\n  codesynapse duplicates ~/src/api ~/src/worker ~/src/cli -o json\n  codesynapse duplicates . --max-percent 5   # fail a CI job above 5%").
		WithGroupID(app.GroupBase).
		Build()
}

// sbomOptions configures `codesynapse sbom`.
type sbomOptions struct {
	approvedPolicyDigest string
	EC                   *app.Env
	vulns                bool
	file                 string
	root                 string
}

func (o *sbomOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.approvedPolicyDigest, "approved-policy-digest", "", "Approve the exact repository policy digest for ingestion exclusions")
	f := o.EC.Command.Flags()
	f.BoolVar(&o.vulns, "vulns", false, "Also look up known vulnerabilities at osv.dev and list them in the SBOM (sends ecosystem, package names and versions)")
	f.StringVar(&o.file, "file", "", "Write the SBOM to this file instead of standard output")
	return nil
}

func (o *sbomOptions) Complete(_ context.Context, _ *app.Env) error {
	o.root = "."
	if len(o.EC.Args) > 0 {
		o.root = o.EC.Args[0]
	}
	abs, err := filepath.Abs(o.root)
	o.root = abs
	return err
}

func (o *sbomOptions) Validate(context.Context, *app.Env) error { return nil }

func (o *sbomOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	repo := filepath.Base(o.root)
	g, err := ingest.Run(ctx, ingest.Options{ApprovedPolicyDigest: o.approvedPolicyDigest, Repo: repo, Root: o.root, VulnScan: o.vulns})
	if err != nil {
		return err
	}
	mem := memory.New()
	if err := mem.ReplaceRepo(ctx, repo, g); err != nil {
		return err
	}
	n, err := mem.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return err
	}
	deps, err := mem.DependencyGraph(ctx, repo)
	if err != nil {
		return err
	}
	b, err := sbom.Marshal(sbom.Build(n, deps, sbom.Options{Tool: version}))
	if err != nil {
		return err
	}
	if o.file != "" {
		return os.WriteFile(o.file, b, 0o644) //nolint:gosec // an SBOM is meant to be shared
	}
	_, err = o.EC.Stdout.Write(b)
	return err
}

func sbomCmd(ec *app.Env) *cobra.Command {
	o := &sbomOptions{EC: ec}
	return app.NewSubCommand("sbom [path]", o, ec).
		WithShortDesc("Write a CycloneDX software bill of materials from go.mod and the lockfiles (offline)").
		WithLongDesc("Lists the dependencies that go.mod and the npm, Python, Rust, PHP, Ruby, .NET and Gradle lockfiles resolve, with licences, versions, package URLs and which module requires which. It is the resolved dependency set, not the contents of a built artifact. --vulns adds known vulnerabilities from osv.dev.").
		WithExample("  codesynapse sbom . --file sbom.cdx.json").
		WithGroupID(app.GroupBase).
		WithMaxArgs(1).
		Build()
}
