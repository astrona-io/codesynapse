// Command codesynapse-api serves the JSON API used by the web dashboard
// (serve) and runs jobs: in the same process, or in workers (worker).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/pgstore"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/auditarchive"
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
		func(ec *app.Env) []*cobra.Command {
			return []*cobra.Command{serveCmd(ec), workerCmd(ec), designateRootCmd(ec), resetAdminCmd(ec)}
		}))
}

type serveOptions struct {
	EC          *app.Env
	graph       app.GraphFlags
	addr        string
	sources     string
	commitLimit int
	network     bool
	moduleProxy bool
	vulnScan    bool
	licenses    bool
	dataflow    bool
	workspace   string
	llmConfig   string
	// worker runs jobs of roles only (codesynapse-api worker); runJobs lets
	// serve run jobs itself, as it does without workers.
	worker  bool
	roles   []string
	runJobs bool
}

func (o *serveOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.graph.AddFlags(f)
	if o.worker {
		f.StringVar(&o.addr, "addr", app.Getenv("CODESYNAPSE_WORKER_ADDR", "127.0.0.1:8081"), "Listen address of the health endpoint")
		f.StringSliceVar(&o.roles, "roles", strings.Split(app.Getenv("CODESYNAPSE_WORKER_ROLES", strings.Join(api.JobRoles(), ",")), ","), "Job roles to run: ingest, ai, fix")
	} else {
		f.StringVar(&o.addr, "addr", app.Getenv("CODESYNAPSE_API_ADDR", "127.0.0.1:8080"), "Listen address")
		f.BoolVar(&o.runJobs, "run-jobs", app.Getenv("CODESYNAPSE_RUN_JOBS", "true") == "true", "Run jobs in this process; false records them for codesynapse-api worker")
	}
	f.StringVar(&o.sources, "sources-root", app.Getenv("CODESYNAPSE_SOURCES_ROOT", ""), "Directory whose subdirectories may be ingested")
	f.IntVar(&o.commitLimit, "commits", 500, "Commits to index per ingest")
	f.StringVar(&o.llmConfig, "llm-config", app.Getenv("CODESYNAPSE_LLM_CONFIG", "config/llm.yaml"), "AI provider configuration (YAML); optional")
	f.StringVar(&o.workspace, "workspace", app.Getenv("CODESYNAPSE_WORKSPACE", ""), "Writable directory for repositories registered by git URL")
	f.BoolVar(&o.vulnScan, "vulns", app.Getenv("CODESYNAPSE_VULN_SCAN", "") == "true", "Check dependencies against osv.dev on ingest (sends module paths and versions)")
	f.BoolVar(&o.licenses, "license-lookup", app.Getenv("CODESYNAPSE_LICENSE_LOOKUP", "") == "true", "Ask deps.dev for dependency licences not found on disk, which dependencies are deprecated or behind their latest release and the OpenSSF Scorecard of their source projects, and the Go module proxy for retracted Go module versions (sends package names and versions)")
	f.BoolVar(&o.dataflow, "dataflow", app.Getenv("CODESYNAPSE_INGEST_DATAFLOW", "") == "true", "Resolve Go calls through function values with SSA and VTA on ingest (slower; edges marked dataflow)")
	f.BoolVar(&o.moduleProxy, "go-module-download", app.Getenv("CODESYNAPSE_GO_MODULE_DOWNLOAD", "true") != "false", "Let ingest fetch missing Go modules from the public module proxy only, so dependencies are type-checked")
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
	if o.worker && os.Getenv(EnvDatabaseURL) == "" {
		return fmt.Errorf("a worker takes its jobs from the accounts database: set %s", EnvDatabaseURL)
	}
	// With accounts every request is authenticated; without them the one
	// bearer token is the only lock.
	if os.Getenv(EnvToken) == "" && os.Getenv(EnvDatabaseURL) == "" && !app.IsLoopback(o.addr) {
		return fmt.Errorf("%w (%s)", app.ErrTokenRequired, EnvToken)
	}
	return nil
}

func (o *serveOptions) Run(ctx context.Context, _ *app.Env) error {
	service := "api"
	if o.worker {
		service = "worker"
	}
	ctx, stopLogs, err := app.StartLogs(ctx, o.EC, service, version)
	if err != nil {
		return err
	}
	defer stopLogs()
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
	if len(llmCfg.Providers) == 0 && os.Getenv("CODESYNAPSE_LLM_DISCOVER") != "false" {
		// Nothing configured: use a local model server if one answers.
		urls := llm.DiscoverURLs
		if v := os.Getenv("CODESYNAPSE_LLM_DISCOVER_URLS"); v != "" {
			urls = strings.Split(v, ",")
		}
		dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if p, ok := llm.DiscoverProvider(dctx, urls); ok {
			if err := llmCfg.Add(p, os.Getenv); err != nil {
				cancel()
				return fmt.Errorf("discovered AI provider: %w", err)
			}
			o.EC.Logger.Info("found a local model server", "base_url", p.BaseURL)
		}
		cancel()
	}
	var registry *llm.Registry
	if len(llmCfg.Providers) > 0 {
		registry = llm.NewRegistry(llmCfg)
		if registry.HasAuto() {
			// First answer before serving, then every five minutes, so a
			// model pulled or removed later is used without a restart.
			rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			for _, c := range registry.Resolve(rctx) {
				o.EC.Logger.Info("AI model picked", "change", c)
			}
			cancel()
			go registry.ResolveEvery(ctx, 5*time.Minute, func(c string) { o.EC.Logger.Info("AI model picked", "change", c) })
		}
		if registry.HasPriceFeed() {
			// Opt-in (price_feed): OpenRouter's public list, once a day.
			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			for _, p := range registry.RefreshPrices(pctx) {
				o.EC.Logger.Warn("AI prices not updated", "problem", p)
			}
			cancel()
			go registry.RefreshPricesEvery(ctx, 24*time.Hour, func(p string) { o.EC.Logger.Warn("AI prices not updated", "problem", p) })
		}
		o.EC.Logger.Info("AI providers configured", "providers", len(llmCfg.Providers), "default", llmCfg.Default)
		if !o.worker {
			go warnMissingModels(ctx, o.EC.Logger, registry, llmCfg)
		}
	}
	trusted, err := app.ParsePrefixes(os.Getenv("CODESYNAPSE_TRUSTED_PROXIES"))
	if err != nil {
		return fmt.Errorf("CODESYNAPSE_TRUSTED_PROXIES: %w", err)
	}
	stopMetrics, err := app.StartMetrics(ctx, o.EC.Logger, service, version)
	if err != nil {
		return err
	}
	defer stopMetrics()
	stopTracing, err := app.StartTracing(ctx, o.EC.Logger, service, version)
	if err != nil {
		return err
	}
	defer stopTracing()
	acc, closeAccounts, err := openAccounts(ctx, o.EC.Logger)
	if err != nil {
		return err
	}
	defer closeAccounts()
	if acc != nil && !o.worker {
		if err := ensureInitialAdmin(ctx, acc, o.EC.Logger); err != nil {
			return err
		}
	}
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
	pushAuths, err := gitx.ParseAuths(os.Getenv("CODESYNAPSE_GIT_PUSH_TOKENS"))
	if err != nil {
		return fmt.Errorf("CODESYNAPSE_GIT_PUSH_TOKENS: %w", err)
	}
	var auditArchive auditarchive.Archive
	if acc != nil && !o.worker { // the API's maintenance loop seals; workers never touch it
		if auditArchive, err = auditarchive.FromEnv(os.Getenv); err != nil {
			return err
		}
	}
	var roles []string // all: this process runs every job
	switch {
	case o.worker:
		roles = o.roles
	case !o.runJobs:
		roles = []string{} // none: workers run them
	}
	srv := api.New(ctx, api.Config{JobRoles: roles, Worker: o.worker, GitAuths: gitAuths, PushAuths: pushAuths, LLM: registry, SourcesRoot: o.sources, CommitLimit: o.commitLimit, AllowNetwork: o.network, ModuleProxy: o.moduleProxy, VulnScan: o.vulnScan, LicenseLookup: o.licenses, Dataflow: o.dataflow, Version: version,
		Workspace: o.workspace, AllowPrivateGit: app.Getenv("CODESYNAPSE_ALLOW_PRIVATE_GIT", "") == "true", Gateway: app.Getenv("CODESYNAPSE_GATEWAY", "true") != "false",
		GitAuth:   gitx.Auth{Host: app.Getenv("CODESYNAPSE_GIT_TOKEN_HOST", "github.com"), Token: os.Getenv("CODESYNAPSE_GIT_TOKEN")},
		AllowPush: pushAllowed(app.Getenv("CODESYNAPSE_ALLOW_PUSH", "") == "true", o.worker, roles, o.EC.Logger),
		Accounts:  acc, ServiceToken: os.Getenv(EnvToken), SecureCookies: app.Getenv("CODESYNAPSE_COOKIE_SECURE", "true") == "true",
		AuditRetention: time.Duration(retentionDays()) * 24 * time.Hour, AuditHot: time.Duration(hotDays()) * 24 * time.Hour, AuditArchive: auditArchive,
		Metrics: metricsQuerier(), IngestMetrics: ingestMetrics,
		TrustedProxies: trusted, PublicMCPURL: app.Getenv("CODESYNAPSE_PUBLIC_MCP_URL", "http://localhost:8090/mcp"),
		PublicAPIURL:   app.Getenv("CODESYNAPSE_PUBLIC_API_URL", ""),
		AgentRulesFile: os.Getenv("CODESYNAPSE_AGENT_RULES_FILE"),
		PublicURL:      app.Getenv("CODESYNAPSE_PUBLIC_URL", "http://localhost:3000"), SSOAllowPrivate: app.Getenv("CODESYNAPSE_SSO_ALLOW_PRIVATE", "") == "true",
		GitAuthor: gitx.Author{Name: app.Getenv("CODESYNAPSE_GIT_AUTHOR_NAME", "CodeSynapse"), Email: app.Getenv("CODESYNAPSE_GIT_AUTHOR_EMAIL", "codesynapse@localhost")}}, st, o.EC.Logger)
	if o.worker {
		// Jobs only; the health endpoint is for probes.
		o.EC.Logger.Info("worker running", "roles", roles, "health", o.addr)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }) //nolint:errcheck // health probe
		return app.Serve(ctx, app.NewServer(o.addr, mux))
	}
	o.EC.Logger.Info("api listening", "addr", o.addr, "sources_root", o.sources, "auth", os.Getenv(EnvToken) != "", "accounts", acc != nil, "run_jobs", o.runJobs)
	if acc != nil {
		return app.Serve(ctx, app.NewServer(o.addr, srv)) // the server authenticates every request itself
	}
	return app.Serve(ctx, app.NewServer(o.addr, app.RequireBearer(os.Getenv(EnvToken), srv)))
}

// warnMissingModels names configured models their server does not offer,
// such as an embedding model nobody pulled, so recall by meaning does not
// fail quietly later. Servers that cannot be reached are left to the
// provider test and the jobs.
func warnMissingModels(ctx context.Context, logger *slog.Logger, r *llm.Registry, cfg llm.Config) {
	for _, p := range cfg.Providers {
		if p.Kind != llm.KindOpenAI || p.Auto() {
			// An auto provider uses what its server has.
			continue
		}
		served, err := r.Models(ctx, p.Name)
		if err != nil {
			continue
		}
		for _, m := range llm.MissingModels(p, served) {
			logger.Warn("AI model not on its server: pull it (Ollama: ollama pull <model>) or fix config/llm.yaml", "provider", p.Name, "model", m)
		}
	}
}

func workerCmd(ec *app.Env) *cobra.Command {
	o := &serveOptions{EC: ec, worker: true}
	return app.NewSubCommand("worker", o, ec).
		WithShortDesc("Run jobs (clone, sync, index, AI tasks, fixes) that the API records").
		WithLongDesc("Claims queued jobs of its roles from the accounts database and runs them as the person who asked, with their access checked again. Several workers share the work; a job a stopped worker left behind is picked up after its lease expires. Run the API with --run-jobs=false (CODESYNAPSE_RUN_JOBS=false) so it only records jobs.").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
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

// EnvAdminPassword is the password of the administrator made at first start
// (docs/operations/first-run.md). docker compose and the Helm chart provide
// a random one; unset, a random one is logged once.
const EnvAdminPassword = "CODESYNAPSE_ADMIN_PASSWORD" //nolint:gosec // env var name

// EnvAutoAdmin turns the first-start administrator off ("false"): the first
// sign-up then becomes root, as before.
const EnvAutoAdmin = "CODESYNAPSE_AUTO_ADMIN"

// ensureInitialAdmin makes the first-start administrator of an empty
// installation. A generated password is logged once, here and nowhere else;
// a supplied one never is.
func ensureInitialAdmin(ctx context.Context, svc *accounts.Service, logger *slog.Logger) error {
	if app.Getenv(EnvAutoAdmin, "true") == "false" {
		return nil
	}
	password := os.Getenv(EnvAdminPassword)
	if strings.HasPrefix(password, "change-me") {
		// The placeholder of .env.example is public: never a real password.
		logger.Warn(EnvAdminPassword + " is still the placeholder from .env.example: generating a random password instead")
		password = ""
	}
	u, generated, err := svc.EnsureInitialAdmin(ctx, password)
	if err != nil {
		return fmt.Errorf("creating the initial administrator: %w", err)
	}
	switch {
	case generated != "":
		logger.Warn("initial administrator created: sign in, then replace it with your own account", "username", u.Username, "password", generated)
	case u.ID != "":
		logger.Warn("initial administrator created with the password from "+EnvAdminPassword+": sign in, then replace it with your own account", "username", u.Username)
	}
	return nil
}

type resetAdminOptions struct {
	EC       *app.Env
	generate bool
}

func (o *resetAdminOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	o.EC.Command.Flags().BoolVar(&o.generate, "generate", false, "Ignore "+EnvAdminPassword+" and print a new random password")
	return nil
}

func (o *resetAdminOptions) Complete(context.Context, *app.Env) error { return nil }

func (o *resetAdminOptions) Validate(context.Context, *app.Env) error {
	if os.Getenv(EnvDatabaseURL) == "" {
		return fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	return nil
}

// Run resets the root account's password (or creates the initial
// administrator on an empty installation). The new password is printed once
// when it was generated; otherwise it is the one in CODESYNAPSE_ADMIN_PASSWORD.
func (o *resetAdminOptions) Run(ctx context.Context, _ *app.Env) error {
	svc, closeDB, err := openAccounts(ctx, o.EC.Logger)
	if err != nil {
		return err
	}
	defer closeDB()
	password := os.Getenv(EnvAdminPassword)
	if o.generate {
		password = ""
	}
	u, generated, err := svc.ResetRoot(ctx, password)
	if err != nil {
		return err
	}
	o.EC.Logger.Info("root account reset", "username", u.Username, "email", u.Email)
	if generated != "" {
		_, err = fmt.Fprintf(os.Stdout, "username: %s\npassword: %s\n", u.Username, generated)
	} else {
		_, err = fmt.Fprintf(os.Stdout, "username: %s\npassword: the value of %s\n", u.Username, EnvAdminPassword)
	}
	return err
}

func resetAdminCmd(ec *app.Env) *cobra.Command {
	o := &resetAdminOptions{EC: ec}
	return app.NewSubCommand("reset-admin", o, ec).
		WithShortDesc("Reset the root (admin) account's password and sessions").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
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

// hotDays reads CODESYNAPSE_AUDIT_HOT_DAYS (default 90): how long entries
// stay in PostgreSQL when an audit archive is configured.
func hotDays() int {
	n, err := strconv.Atoi(app.Getenv("CODESYNAPSE_AUDIT_HOT_DAYS", "90"))
	if err != nil || n < 7 {
		return 90
	}
	return n
}

// pushAllowed is whether this process may push fix branches. The API needs
// the switch to accept a fix request; a worker needs it only when it has the
// fix role. A worker without that role parses untrusted repositories and
// never holds the switch, whatever its environment says.
func pushAllowed(wanted, worker bool, roles []string, logger *slog.Logger) bool {
	if !wanted || !worker || slices.Contains(roles, "fix") {
		return wanted
	}
	logger.Warn("CODESYNAPSE_ALLOW_PUSH ignored: this worker has no fix role", "roles", roles)
	return false
}
