// Command codesynapse-api serves the JSON API used by the web dashboard and
// runs ingest jobs for repositories under the sources root.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/parisnakitakejser/codesynapse/internal/app"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/api"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/llm"
)

var version = "dev"

// EnvToken is the bearer token required by the API.
const EnvToken = "CODESYNAPSE_API_TOKEN" //nolint:gosec // env var name

func main() {
	os.Exit(app.Main("codesynapse-api", "HTTP API for the codesynapse dashboard", version,
		func(ec *app.Env) []*cobra.Command { return []*cobra.Command{serveCmd(ec)} }))
}

type serveOptions struct {
	EC          *app.Env
	neo4j       app.Neo4jFlags
	addr        string
	sources     string
	commitLimit int
	network     bool
	vulnScan    bool
	licenses    bool
	workspace   string
	llmConfig   string
}

func (o *serveOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.neo4j.AddFlags(f)
	f.StringVar(&o.addr, "addr", app.Getenv("CODESYNAPSE_API_ADDR", "127.0.0.1:8080"), "Listen address")
	f.StringVar(&o.sources, "sources-root", app.Getenv("CODESYNAPSE_SOURCES_ROOT", ""), "Directory whose subdirectories may be ingested")
	f.IntVar(&o.commitLimit, "commits", 500, "Commits to index per ingest")
	f.StringVar(&o.llmConfig, "llm-config", app.Getenv("CODESYNAPSE_LLM_CONFIG", "config/llm.yaml"), "AI provider configuration (YAML); optional")
	f.StringVar(&o.workspace, "workspace", app.Getenv("CODESYNAPSE_WORKSPACE", ""), "Writable directory for repositories registered by git URL")
	f.BoolVar(&o.vulnScan, "vulns", app.Getenv("CODESYNAPSE_VULN_SCAN", "") == "true", "Check dependencies against osv.dev on ingest (sends module paths and versions)")
	f.BoolVar(&o.licenses, "license-lookup", app.Getenv("CODESYNAPSE_LICENSE_LOOKUP", "") == "true", "Look up dependency licences not found on disk on deps.dev (sends package names and versions)")
	f.BoolVar(&o.network, "allow-network", app.Getenv("CODESYNAPSE_ALLOW_NETWORK", "") == "true", "Let ingest download Go modules (trusted repos only)")
	return nil
}

func (o *serveOptions) Complete(context.Context, *app.Env) error {
	if o.workspace != "" {
		abs, err := filepath.Abs(o.workspace)
		if err != nil {
			return err
		}
		o.workspace = abs
	}
	if o.sources == "" {
		return nil
	}
	abs, err := filepath.Abs(o.sources)
	o.sources = abs
	return err
}

func (o *serveOptions) Validate(context.Context, *app.Env) error {
	if os.Getenv(EnvToken) == "" && !app.IsLoopback(o.addr) {
		return fmt.Errorf("%w (%s)", app.ErrTokenRequired, EnvToken)
	}
	return nil
}

func (o *serveOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	st, err := o.neo4j.OpenWithRetry(ctx, func(err error) { o.EC.Logger.Warn("waiting for neo4j", "error", err) })
	if err != nil {
		return err
	}
	defer st.Close(context.Background()) //nolint:errcheck,contextcheck // shutdown
	if err := st.EnsureSchema(ctx); err != nil {
		return err
	}
	llmCfg, err := llm.Load(o.llmConfig, os.Getenv)
	if err != nil {
		return fmt.Errorf("AI provider config: %w", err)
	}
	var registry *llm.Registry
	if len(llmCfg.Providers) > 0 {
		registry = llm.NewRegistry(llmCfg)
		o.EC.Logger.Info("AI providers configured", "providers", len(llmCfg.Providers), "default", llmCfg.Default)
	}
	gitAuths, err := gitx.ParseAuths(os.Getenv("CODESYNAPSE_GIT_TOKENS"))
	if err != nil {
		return fmt.Errorf("CODESYNAPSE_GIT_TOKENS: %w", err)
	}
	srv := api.New(ctx, api.Config{GitAuths: gitAuths, LLM: registry, SourcesRoot: o.sources, CommitLimit: o.commitLimit, AllowNetwork: o.network, VulnScan: o.vulnScan, LicenseLookup: o.licenses, Version: version,
		Workspace: o.workspace, AllowPrivateGit: app.Getenv("CODESYNAPSE_ALLOW_PRIVATE_GIT", "") == "true",
		GitAuth:   gitx.Auth{Host: app.Getenv("CODESYNAPSE_GIT_TOKEN_HOST", "github.com"), Token: os.Getenv("CODESYNAPSE_GIT_TOKEN")},
		AllowPush: app.Getenv("CODESYNAPSE_ALLOW_PUSH", "") == "true",
		GitAuthor: gitx.Author{Name: app.Getenv("CODESYNAPSE_GIT_AUTHOR_NAME", "CodeSynapse"), Email: app.Getenv("CODESYNAPSE_GIT_AUTHOR_EMAIL", "codesynapse@localhost")}}, st, o.EC.Logger)
	o.EC.Logger.Info("api listening", "addr", o.addr, "sources_root", o.sources, "auth", os.Getenv(EnvToken) != "")
	return app.Serve(ctx, app.NewServer(o.addr, app.RequireBearer(os.Getenv(EnvToken), srv)))
}

func serveCmd(ec *app.Env) *cobra.Command {
	o := &serveOptions{EC: ec}
	return app.NewSubCommand("serve", o, ec).
		WithShortDesc("Run the API server").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}
