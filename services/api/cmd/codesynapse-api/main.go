// Command codesynapse-api serves the JSON API used by the web dashboard and
// runs ingest jobs for repositories under the sources root.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/pgstore"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/api"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

var version = "dev"

// EnvToken is the bearer token required by the API.
const EnvToken = "CODESYNAPSE_API_TOKEN" //nolint:gosec // env var name

func main() {
	os.Exit(app.Main("codesynapse-api", "HTTP API for the codesynapse dashboard", version,
		func(ec *app.Env) []*cobra.Command { return []*cobra.Command{serveCmd(ec), designateRootCmd(ec)} }))
}

type serveOptions struct {
	EC          *app.Env
	graph       app.GraphFlags
	addr        string
	sources     string
	commitLimit int
	network     bool
	vulnScan    bool
	licenses    bool
	dataflow    bool
	workspace   string
	llmConfig   string
}

func (o *serveOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.graph.AddFlags(f)
	f.StringVar(&o.addr, "addr", app.Getenv("CODESYNAPSE_API_ADDR", "127.0.0.1:8080"), "Listen address")
	f.StringVar(&o.sources, "sources-root", app.Getenv("CODESYNAPSE_SOURCES_ROOT", ""), "Directory whose subdirectories may be ingested")
	f.IntVar(&o.commitLimit, "commits", 500, "Commits to index per ingest")
	f.StringVar(&o.llmConfig, "llm-config", app.Getenv("CODESYNAPSE_LLM_CONFIG", "config/llm.yaml"), "AI provider configuration (YAML); optional")
	f.StringVar(&o.workspace, "workspace", app.Getenv("CODESYNAPSE_WORKSPACE", ""), "Writable directory for repositories registered by git URL")
	f.BoolVar(&o.vulnScan, "vulns", app.Getenv("CODESYNAPSE_VULN_SCAN", "") == "true", "Check dependencies against osv.dev on ingest (sends module paths and versions)")
	f.BoolVar(&o.licenses, "license-lookup", app.Getenv("CODESYNAPSE_LICENSE_LOOKUP", "") == "true", "Look up dependency licences not found on disk on deps.dev (sends package names and versions)")
	f.BoolVar(&o.dataflow, "dataflow", app.Getenv("CODESYNAPSE_INGEST_DATAFLOW", "") == "true", "Resolve Go calls through function values with SSA and VTA on ingest (slower; edges marked dataflow)")
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
	// With accounts every request is authenticated; without them the one
	// bearer token is the only lock.
	if os.Getenv(EnvToken) == "" && os.Getenv(EnvDatabaseURL) == "" && !app.IsLoopback(o.addr) {
		return fmt.Errorf("%w (%s)", app.ErrTokenRequired, EnvToken)
	}
	return nil
}

func (o *serveOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	st, err := o.graph.OpenWithRetry(ctx, func(err error) { o.EC.Logger.Warn("waiting for the graph database", "error", err) })
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
	trusted, err := app.ParsePrefixes(os.Getenv("CODESYNAPSE_TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("CODESYNAPSE_TRUSTED_PROXIES: %w", err)
	}
	stopMetrics, err := app.StartMetrics(ctx, o.EC.Logger, "api", version)
	if err != nil {
		return err
	}
	defer stopMetrics()
	acc, closeAccounts, err := openAccounts(ctx, o.EC.Logger)
	if err != nil {
		return err
	}
	defer closeAccounts()
	ingestMetrics, err := app.NewIngestMetrics(ctx)
	if err != nil {
		return err
	}
	if ingestMetrics != nil {
		defer func() {
			shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := ingestMetrics.Shutdown(shutdown); err != nil {
				o.EC.Logger.Warn("ingest metrics shutdown failed", "error", err)
			}
		}()
	}
	gitAuths, err := gitx.ParseAuths(os.Getenv("CODESYNAPSE_GIT_TOKENS"))
	if err != nil {
		return fmt.Errorf("CODESYNAPSE_GIT_TOKENS: %w", err)
	}
	srv := api.New(ctx, api.Config{GitAuths: gitAuths, LLM: registry, SourcesRoot: o.sources, CommitLimit: o.commitLimit, AllowNetwork: o.network, VulnScan: o.vulnScan, LicenseLookup: o.licenses, Dataflow: o.dataflow, Version: version,
		Workspace: o.workspace, AllowPrivateGit: app.Getenv("CODESYNAPSE_ALLOW_PRIVATE_GIT", "") == "true",
		GitAuth:   gitx.Auth{Host: app.Getenv("CODESYNAPSE_GIT_TOKEN_HOST", "github.com"), Token: os.Getenv("CODESYNAPSE_GIT_TOKEN")},
		AllowPush: app.Getenv("CODESYNAPSE_ALLOW_PUSH", "") == "true",
		Accounts:  acc, ServiceToken: os.Getenv(EnvToken), SecureCookies: app.Getenv("CODESYNAPSE_COOKIE_SECURE", "true") == "true",
		AuditRetention: time.Duration(retentionDays()) * 24 * time.Hour,
		Metrics:        metricsQuerier(), IngestMetrics: ingestMetrics,
		TrustedProxies: trusted, PublicMCPURL: app.Getenv("CODESYNAPSE_PUBLIC_MCP_URL", "http://localhost:8090/mcp"),
		PublicURL: app.Getenv("CODESYNAPSE_PUBLIC_URL", "http://localhost:3000"), SSOAllowPrivate: app.Getenv("CODESYNAPSE_SSO_ALLOW_PRIVATE", "") == "true",
		GitAuthor: gitx.Author{Name: app.Getenv("CODESYNAPSE_GIT_AUTHOR_NAME", "CodeSynapse"), Email: app.Getenv("CODESYNAPSE_GIT_AUTHOR_EMAIL", "codesynapse@localhost")}}, st, o.EC.Logger)
	o.EC.Logger.Info("api listening", "addr", o.addr, "sources_root", o.sources, "auth", os.Getenv(EnvToken) != "", "accounts", acc != nil)
	if acc != nil {
		return app.Serve(ctx, app.NewServer(o.addr, srv)) // the server authenticates every request itself
	}
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

// EnvDatabaseURL turns on accounts (users, organisations, API keys, audit
// log). It holds a password, so it is read from the environment only.
const EnvDatabaseURL = "CODESYNAPSE_DATABASE_URL" //nolint:gosec // env var name

// EnvSecretKey seals organisation Git tokens (32 bytes, base64 or hex).
// Losing it makes stored tokens unreadable; they then have to be set again.
const EnvSecretKey = "CODESYNAPSE_SECRET_KEY" //nolint:gosec // env var name

// openAccounts connects to PostgreSQL and migrates it, or returns nil when
// no database is configured (single-tenant mode).
func openAccounts(ctx context.Context, logger *slog.Logger) (*accounts.Service, func(), error) {
	url := os.Getenv(EnvDatabaseURL)
	if url == "" {
		return nil, func() {}, nil
	}
	st, err := pgstore.OpenWait(ctx, url, func(attempt int, err error) {
		logger.Warn("waiting for PostgreSQL", "attempt", attempt, "error", err)
	})
	if err != nil {
		return nil, nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("migrating the accounts database: %w", err)
	}
	svc := accounts.NewService(st, app.Getenv("CODESYNAPSE_SIGNUP_OPEN", "") == "true")
	if raw := os.Getenv(EnvSecretKey); raw != "" {
		key, err := accounts.ParseSecretKey(raw)
		if err != nil {
			st.Close()
			return nil, nil, fmt.Errorf("%s: %w", EnvSecretKey, err)
		}
		if svc.Secrets, err = accounts.NewSealer(key); err != nil {
			st.Close()
			return nil, nil, err
		}
	} else {
		logger.Warn("organisation Git tokens are off: set " + EnvSecretKey + " to allow private repositories for organisations")
	}
	setup, err := svc.ReconcileBootstrap(ctx)
	if err != nil {
		st.Close()
		return nil, nil, fmt.Errorf("classifying the bootstrap root: %w", err)
	}
	switch setup.State {
	case accounts.SetupAmbiguous:
		logger.Warn("the bootstrap root could not be identified from setup history: platform administrators are limited to governance until an operator runs `codesynapse-api designate-root --email <root>`")
	case accounts.SetupPending:
		logger.Warn("setup is not complete: sign in as the root account and create an organisation and its owner")
	}
	logger.Info("accounts enabled", "signup_open", svc.SignupOpen, "setup", setup.State)
	return svc, st.Close, nil
}

type designateRootOptions struct {
	EC    *app.Env
	email string
}

func (o *designateRootOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.email, "email", "", "Email of the platform administrator who set up the installation")
	return nil
}

func (o *designateRootOptions) Complete(context.Context, *app.Env) error { return nil }

func (o *designateRootOptions) Validate(context.Context, *app.Env) error {
	if o.email == "" {
		return fmt.Errorf("--email is required")
	}
	if os.Getenv(EnvDatabaseURL) == "" {
		return fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	return nil
}

// Run records the operator's audited decision for an installation whose
// bootstrap root is ambiguous (docs/ROOT_BOOTSTRAP_PLAN.md).
func (o *designateRootOptions) Run(ctx context.Context, _ *app.Env) error {
	svc, closeDB, err := openAccounts(ctx, o.EC.Logger)
	if err != nil {
		return err
	}
	defer closeDB()
	setup, err := svc.DesignateRoot(ctx, o.email)
	if err != nil {
		return err
	}
	o.EC.Logger.Info("bootstrap root designated", "setup", setup.State)
	return nil
}

func designateRootCmd(ec *app.Env) *cobra.Command {
	o := &designateRootOptions{EC: ec}
	return app.NewSubCommand("designate-root", o, ec).
		WithShortDesc("Name the bootstrap root of an installation whose setup history is ambiguous").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}

// metricsQuerier reads the statistics' time series from the VictoriaMetrics
// the services push to, or nil without one.
func metricsQuerier() *telemetry.Querier {
	url := os.Getenv(app.EnvMetricsURL)
	if url == "" || telemetry.CheckURL(url) != nil {
		return nil // StartMetrics already refused a bad URL
	}
	return &telemetry.Querier{URL: url, User: app.Getenv(app.EnvMetricsUser, ""), Password: os.Getenv(app.EnvMetricsPassword)}
}

// retentionDays reads CODESYNAPSE_AUDIT_RETENTION_DAYS (default 400).
func retentionDays() int {
	n, err := strconv.Atoi(app.Getenv("CODESYNAPSE_AUDIT_RETENTION_DAYS", "400"))
	if err != nil || n < 30 {
		return 400 // NIS2 evidence needs history; below a month is a typo
	}
	return n
}
