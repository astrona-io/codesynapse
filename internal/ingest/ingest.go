// Package ingest runs every extractor over a repository and assembles one
// graph ready to persist.
package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/logx"

	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/ingest/config"
	"github.com/parisnakitakejser/codesynapse/internal/ingest/docs"
	"github.com/parisnakitakejser/codesynapse/internal/ingest/githistory"
	"github.com/parisnakitakejser/codesynapse/internal/ingest/golang"
	"github.com/parisnakitakejser/codesynapse/internal/ingest/lang"
	"github.com/parisnakitakejser/codesynapse/internal/metrics"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/vuln"
)

// Options configures an ingest run.
type Options struct {
	Repo string
	Root string
	// CommitLimit is how many recent commits to index; 0 skips history.
	CommitLimit int
	// AllowNetwork lets `go list` download modules.
	AllowNetwork bool
	// VulnScan queries osv.dev for known vulnerabilities in go.mod
	// dependencies and the standard library. Sends module paths and versions.
	VulnScan bool
	// OSV overrides the vulnerability client (tests, mirrors).
	OSV *vuln.Client
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
	if opts.Repo == "" {
		opts.Repo = filepath.Base(root)
	}

	start := time.Now()
	g := model.NewGraph()
	repoNode := g.AddNode(model.Node{
		ID: model.NodeID(opts.Repo, model.KindRepository, opts.Repo), Kind: model.KindRepository,
		Repo: opts.Repo, Name: opts.Repo, Root: root, Head: gitx.HeadCommit(ctx, root),
		Time: start.UTC().Format(time.RFC3339), URL: gitx.SanitizeURL(gitx.OriginURL(ctx, root)),
		Branch: opts.Branch,
	})
	if repoNode.Branch == "" {
		repoNode.Branch = gitx.CurrentBranch(ctx, root)
	}

	extImports := map[string][]string{}
	var goRes *golang.Result
	if hasGoModule(root) {
		res, err := golang.Extract(ctx, golang.Options{Repo: opts.Repo, Root: root, AllowNetwork: opts.AllowNetwork})
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

	docGraph, err := docs.Extract(ctx, opts.Repo, root, g)
	if err != nil {
		return nil, fmt.Errorf("docs extraction: %w", err)
	}
	g.Merge(docGraph)

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
	if err := addDependencies(ctx, opts, root, g, goRes, extImports); err != nil {
		return nil, err
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
	cl := cleanup.Analyze(g, extImports)
	if repoNode.Cleanup, err = cleanup.Encode(cl); err != nil {
		return nil, fmt.Errorf("encoding cleanup report: %w", err)
	}
	rep, err := metrics.Compute(ctx, opts.Repo, g, metrics.Options{})
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	if repoNode.Metrics, err = metrics.Encode(rep); err != nil {
		return nil, fmt.Errorf("encoding metrics: %w", err)
	}
	logger.Info("ingest complete", "repo", opts.Repo, "nodes", len(g.Nodes()), "edges", len(g.Edges()),
		"pruned_edges", pruned, "duration", time.Since(start).Round(time.Millisecond).String())
	return g, nil
}

func hasGoModule(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error { //nolint:errcheck // best-effort probe
		if err != nil || found {
			return filepath.SkipAll
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

// addDependencies records go.mod modules and, when enabled, their known
// vulnerabilities. A failed vulnerability lookup is logged, not fatal: the
// code graph is still useful without it.
func addDependencies(ctx context.Context, opts Options, root string, g *model.Graph, goRes *golang.Result, imports map[string][]string) error {
	logger := logx.FromContext(ctx)
	var mods []vuln.Module
	if goRes != nil {
		goMods, err := vuln.Modules(root, runtime.Version())
		if err != nil {
			return fmt.Errorf("reading go.mod: %w", err)
		}
		mods = goMods
	}
	other, err := vuln.ManifestModules(root)
	if err != nil {
		return fmt.Errorf("reading lockfiles: %w", err)
	}
	mods = append(mods, other...)
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
	vuln.Apply(opts.Repo, g, mods, vulns, calls, imports)
	return nil
}
