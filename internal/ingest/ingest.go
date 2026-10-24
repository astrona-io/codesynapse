// Package ingest runs every extractor over a repository and assembles one
// graph ready to persist.
package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/mod/modfile"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/logx"

	"github.com/astrona-io/codesynapse/internal/arch"
	"github.com/astrona-io/codesynapse/internal/cleanup"
	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/identity"
	"github.com/astrona-io/codesynapse/internal/ingest/config"
	"github.com/astrona-io/codesynapse/internal/ingest/docs"
	"github.com/astrona-io/codesynapse/internal/ingest/githistory"
	"github.com/astrona-io/codesynapse/internal/ingest/golang"
	"github.com/astrona-io/codesynapse/internal/ingest/lang"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/metrics"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/vuln"
)

// Options configures an ingest run.
type Options struct {
	// ApprovedPolicyDigest is supplied by a trusted control plane or CLI operator.
	ApprovedPolicyDigest string
	Repo                 string
	Root                 string
	// PreviousIdentities is read from the stored repository before extraction.
	// A corrupt baseline fails ingest instead of resetting permanent IDs.
	PreviousIdentities string
	// CommitLimit is how many recent commits to index; 0 skips history.
	CommitLimit int
	// AllowNetwork lets `go list` download modules.
	AllowNetwork bool
	// Dataflow adds CALLS edges for Go calls through function values (SSA
	// and VTA). Off by default; see docs/EDGE_EVIDENCE.md.
	Dataflow bool
	// VulnScan queries osv.dev for known vulnerabilities in go.mod
	// dependencies and the standard library. Sends module paths and versions.
	VulnScan bool
	// OSV overrides the vulnerability client (tests, mirrors).
	OSV *vuln.Client
	// LicenseLookup asks deps.dev for dependency licences that are not found
	// on disk. Sends ecosystem, package name and version.
	LicenseLookup bool
	// DepsDev overrides the licence lookup client (tests, mirrors).
	DepsDev *license.DepsDev
	// Branch is recorded on the Repository node; when empty the checked-out
	// branch is used.
	Branch string
}

// Run extracts code, docs and history from opts.Root.
func Run(ctx context.Context, opts Options) (*model.Graph, error) {
	logger := logx.FromContext(ctx)
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("repository root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("repository root %s is not a directory", root)
	}
	ctx, err = preparePolicy(ctx, root, opts)
	if err != nil {
		return nil, err
	}
	policy := repoconfig.FromContext(ctx)
	head := gitx.HeadCommit(ctx, root)
	ledger := ingestreport.FromContext(ctx)
	if err := ledger.SetSource(head, policy.Digest()); err != nil {
		return nil, err
	}
	if err := ledger.Discover(ctx, root, policy); err != nil {
		return nil, fmt.Errorf("discover files: %w", err)
	}
	if err := ledger.Checkpoint(ctx); err != nil {
		return nil, fmt.Errorf("checkpoint ingest inventory: %w", err)
	}
	metadata, err := policyMetadata(ctx, root)
	if err != nil {
		return nil, err
	}
	if opts.Repo == "" {
		opts.Repo = filepath.Base(root)
	}
	previous, err := identity.Decode(opts.PreviousIdentities, opts.Repo)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	g := model.NewGraph()
	repoNode := g.AddNode(model.Node{
		ID: model.NodeID(opts.Repo, model.KindRepository, opts.Repo), Kind: model.KindRepository,
		Repo: opts.Repo, Name: opts.Repo, Root: root, Head: head,
		Time: start.UTC().Format(time.RFC3339), URL: gitx.SanitizeURL(gitx.HTTPSRemote(gitx.OriginURL(ctx, root))),
		Branch: opts.Branch,
	})
	if repoNode.Branch == "" {
		repoNode.Branch = gitx.CurrentBranch(ctx, root)
	}
	// The Go module at the root: answers write it as "~" in node IDs
	// (internal/coretools, compact) and the resolver expands it back.
	meta, err := json.Marshal(repoMetadata{Policy: metadata, GoModule: rootModulePath(root)})
	if err != nil {
		return nil, fmt.Errorf("encode repository policy: %w", err)
	}
	repoNode.Meta = string(meta)
	own := (license.Resolver{Root: root, Policy: policy}).Project()
	repoNode.License, repoNode.LicenseSource = own.License, own.Source

	extImports := map[string][]string{}
	var goRes *golang.Result
	if hasGoModule(root, policy) {
		res, err := golang.Extract(ctx, golang.Options{Repo: opts.Repo, Root: root, AllowNetwork: opts.AllowNetwork, Dataflow: opts.Dataflow})
		if err != nil {
			return nil, fmt.Errorf("go extraction: %w", err)
		}
		code := res.Graph
		g.Merge(code)
		goRes = res
		for k, v := range res.ExternalImports {
			extImports[k] = v
		}
		logger.Info("extracted go code", "nodes", len(code.Nodes()), "edges", len(code.Edges()))
	}

	langRes, err := lang.Extract(ctx, opts.Repo, root)
	if err != nil {
		return nil, fmt.Errorf("python/js/ts extraction: %w", err)
	}
	g.Merge(langRes.Graph)
	for k, v := range langRes.ExternalImports {
		extImports[k] = append(extImports[k], v...)
	}
	if len(langRes.Languages) > 0 {
		logger.Info("extracted source code", "files", langRes.Languages)
	}
	// Documentation is linked last among the code languages: a mention can
	// only resolve to a symbol that is already in the graph.
	docGraph, err := docs.Extract(ctx, opts.Repo, root, g)
	if err != nil {
		return nil, fmt.Errorf("docs extraction: %w", err)
	}
	g.Merge(docGraph)
	if err := arch.Extract(ctx, opts.Repo, root, g); err != nil {
		return nil, fmt.Errorf("architecture records: %w", err)
	}
	mods, err := addDependencies(ctx, opts, root, g, goRes, extImports)
	if err != nil {
		return nil, err
	}
	if repoNode.Licenses, err = license.Encode(license.Build(own, licenseDeps(mods), opts.LicenseLookup)); err != nil {
		return nil, fmt.Errorf("licence report: %w", err)
	}

	cfgGraph, err := config.Extract(ctx, opts.Repo, root)
	if err != nil {
		return nil, fmt.Errorf("yaml/template extraction: %w", err)
	}
	g.Merge(cfgGraph)

	if opts.CommitLimit > 0 && repoNode.Head != "" {
		if err := githistory.Extract(ctx, opts.Repo, root, opts.CommitLimit, g); err != nil {
			// History is enrichment; a shallow clone or odd repo should not
			// block indexing the code.
			logger.Warn("skipping git history", logx.Error(err))
		}
	}

	pruned := g.PruneDanglingEdges()
	repoNode.Analysis = analysisSummary(g).Encode()
	cl := cleanup.Analyze(g, extImports)
	if repoNode.Cleanup, err = cleanup.Encode(cl); err != nil {
		return nil, fmt.Errorf("encoding cleanup report: %w", err)
	}
	if repoNode.Duplicates, err = duplicates.Encode(duplicates.Analyze(g)); err != nil {
		return nil, err
	}
	rep, err := metrics.Compute(ctx, opts.Repo, g, metrics.Options{})
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	if repoNode.Metrics, err = metrics.Encode(rep); err != nil {
		return nil, fmt.Errorf("encoding metrics: %w", err)
	}
	if repoNode.Coverage, err = security.EncodeCoverage(policyCoverage(security.CoverageOf(g, security.Inventory(root, policy)), metadata)); err != nil {
		return nil, fmt.Errorf("encoding security coverage: %w", err)
	}
	identities, err := identity.Reconcile(g, opts.Repo, previous)
	if err != nil {
		return nil, err
	}
	if repoNode.Identities, err = identity.Encode(identities); err != nil {
		return nil, fmt.Errorf("encoding symbol identities: %w", err)
	}
	logger.Info("ingest complete", "repo", opts.Repo, "nodes", len(g.Nodes()), "edges", len(g.Edges()),
		"pruned_edges", pruned, "duration", time.Since(start).Round(time.Millisecond).String())
	return g, nil
}

// rootModulePath is the module path declared by go.mod at root, or "". The
// file is untrusted: a symlink or an oversized file is not read.
func rootModulePath(root string) string {
	p := filepath.Join(root, "go.mod")
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return ""
	}
	data, err := os.ReadFile(p) //nolint:gosec // go.mod at the repository root, checked above
	if err != nil {
		return ""
	}
	return modfile.ModulePath(data)
}

func hasGoModule(root string, policies ...*repoconfig.Policy) bool {
	var policy *repoconfig.Policy
	if len(policies) > 0 {
		policy = policies[0]
	}
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // best-effort probe
		if err != nil || found {
			return filepath.SkipAll
		}
		if allowed, policyErr := policy.WalkEntry(root, path, d); !allowed || policyErr != nil {
			return policyErr
		}
		if d.IsDir() && path != root && (d.Name() == "vendor" || d.Name() == "node_modules" || d.Name() == "testdata" || d.Name()[0] == '.') {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == "go.mod" {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// addDependencies records go.mod and lockfile modules with their licences
// and, when enabled, their known vulnerabilities. A failed vulnerability lookup is logged, not fatal: the
// code graph is still useful without it.
func addDependencies(ctx context.Context, opts Options, root string, g *model.Graph, goRes *golang.Result, imports map[string][]string) ([]vuln.Module, error) {
	logger := logx.FromContext(ctx)
	var mods []vuln.Module
	if goRes != nil {
		goMods, err := vuln.ModulesContext(ctx, root, runtime.Version(), repoconfig.FromContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("reading go.mod: %w", err)
		}
		mods = goMods
	}
	other, err := vuln.ManifestModulesContext(ctx, root, repoconfig.FromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("reading lockfiles: %w", err)
	}
	mods = append(mods, other...)
	var goEdges map[string][]string
	if goRes != nil {
		goEdges = vuln.GoGraph(ctx, root, opts.AllowNetwork)
	}
	vuln.AssignPaths(mods, goEdges)
	var calls map[string][]string
	if goRes != nil {
		calls = goRes.ExternalCalls
	}
	var vulns []vuln.Vulnerability
	if opts.VulnScan {
		client := opts.OSV
		if client == nil {
			client = &vuln.Client{}
		}
		vulns, err = client.Scan(ctx, mods)
		if err != nil {
			logger.Warn("vulnerability scan failed", logx.Error(err))
			vulns = nil
		} else {
			logger.Info("vulnerability scan complete", "modules", len(mods), "vulnerabilities", len(vulns))
		}
	}
	resolveLicenses(ctx, opts, root, mods)
	vuln.Apply(opts.Repo, g, mods, vulns, calls, imports)
	return mods, nil
}

// resolveLicenses fills in licences the lockfiles did not record.
func resolveLicenses(ctx context.Context, opts Options, root string, mods []vuln.Module) {
	var deps []license.Dep
	for _, m := range mods {
		if m.License == "" {
			deps = append(deps, license.Dep{Ecosystem: cmp.Or(m.Ecosystem, vuln.EcosystemGo), Name: m.Path, Version: m.Version})
		}
	}
	r := license.Resolver{Root: root, Policy: repoconfig.FromContext(ctx)}
	if opts.LicenseLookup {
		r.Online = cmp.Or(opts.DepsDev, &license.DepsDev{})
	}
	found := r.Resolve(ctx, deps)
	for i, m := range mods {
		d := license.Dep{Ecosystem: cmp.Or(m.Ecosystem, vuln.EcosystemGo), Name: m.Path, Version: m.Version}
		if f, ok := found[d.Key()]; ok && m.License == "" {
			mods[i].License, mods[i].LicenseSource = f.License, f.Source
		}
	}
	logx.FromContext(ctx).Info("licences resolved", "dependencies", len(mods), "found", len(found), "online", opts.LicenseLookup)
}

// Licenses checks the dependency licences of the repository at root without
// indexing its code: the quick path for `codesynapse licenses` in CI.
func Licenses(ctx context.Context, root string, opts Options) (license.Report, error) {
	var err error
	ctx, err = preparePolicy(ctx, root, opts)
	if err != nil {
		return license.Report{}, err
	}
	var mods []vuln.Module
	if hasGoModule(root, repoconfig.FromContext(ctx)) {
		goMods, err := vuln.ModulesContext(ctx, root, runtime.Version(), repoconfig.FromContext(ctx))
		if err != nil {
			return license.Report{}, fmt.Errorf("reading go.mod: %w", err)
		}
		mods = goMods
	}
	other, err := vuln.ManifestModulesContext(ctx, root, repoconfig.FromContext(ctx))
	if err != nil {
		return license.Report{}, fmt.Errorf("reading lockfiles: %w", err)
	}
	mods = append(mods, other...)
	resolveLicenses(ctx, opts, root, mods)
	own := (license.Resolver{Root: root, Policy: repoconfig.FromContext(ctx)}).Project()
	return license.Build(own, licenseDeps(mods), opts.LicenseLookup), nil
}

func licenseDeps(mods []vuln.Module) []license.Dependency {
	deps := make([]license.Dependency, len(mods))
	for i, m := range mods {
		deps[i] = license.Dependency{Ecosystem: cmp.Or(m.Ecosystem, vuln.EcosystemGo), Name: m.BuildPath(), Version: m.Version,
			License: m.License, Source: m.LicenseSource, Indirect: m.Indirect, Dev: m.Dev}
	}
	return deps
}
