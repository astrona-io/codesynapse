// Package api is the HTTP JSON API behind the web dashboard. It serves the
// same query answers as the MCP server plus dashboard aggregates, and runs
// ingest jobs for repositories under a configured sources root.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/services/api/internal/annotation"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
	"github.com/astrona-io/codesynapse/services/api/internal/offload"
	"github.com/astrona-io/codesynapse/services/api/internal/recall"
)

const maxBodyBytes = 4 << 20

// Config configures the API.
type Config struct {
	// SourcesRoot is the only directory tree ingest may read. Requests name a
	// child directory; anything resolving outside the root is rejected.
	SourcesRoot string
	// CommitLimit is passed to ingest runs.
	CommitLimit int
	// AllowNetwork lets ingest download Go modules. Off by default; enable
	// only for repositories you trust.
	AllowNetwork bool
	// VulnScan checks dependencies against osv.dev during ingest.
	VulnScan bool
	// LicenseLookup asks deps.dev for licences not found on disk.
	LicenseLookup bool
	// Dataflow runs the optional Go data-flow call analysis on ingest.
	Dataflow bool
	// Accounts enables users, organisations and API keys (PostgreSQL);
	// nil keeps the single-tenant mode behind one bearer token.
	Accounts *accounts.Service
	// ServiceToken, in accounts mode, still authenticates internal callers
	// (the web app, MCP, hooks) as a platform admin.
	ServiceToken string
	// SecureCookies marks the session cookie Secure (HTTPS deployments).
	SecureCookies bool
	// AuditRetention is how long audit entries are kept (default 400 days).
	AuditRetention time.Duration
	// Metrics reads time series for the statistics (nil without
	// VictoriaMetrics).
	Metrics       *telemetry.Querier
	IngestMetrics *telemetry.IngestExporter
	// TrustedProxies may report the client address in X-Forwarded-For
	// (the web app); everyone else is identified by the connection.
	TrustedProxies []netip.Prefix
	// PublicMCPURL is where agents reach the MCP server, shown in the
	// connection instructions.
	PublicMCPURL string
	// PublicURL is the dashboard's address; single sign-on returns to
	// PublicURL + "/sso/callback".
	PublicURL string
	// SSOAllowPrivate lets organisations use an identity provider on a
	// private network (self-hosted Keycloak and the like).
	SSOAllowPrivate bool
	// SSOClient replaces the guarded identity-provider client (tests).
	SSOClient *http.Client
	// Workspace is a writable directory for repositories cloned by URL.
	Workspace string
	// GitAuth is an optional token for cloning private repositories.
	GitAuth gitx.Auth
	// GitAuths holds tokens for further hosts (CODESYNAPSE_GIT_TOKENS). A token
	// is only ever sent to its own host.
	GitAuths []gitx.Auth
	// AllowPrivateGit permits remotes on private networks.
	AllowPrivateGit bool
	// AllowPush lets CodeSynapse push fix branches for accepted review
	// comments and open pull requests. Off by default.
	AllowPush bool
	// GitAuthor signs fix commits.
	GitAuthor gitx.Author
	// ForgeAPI overrides the REST endpoint used to list organisations, for every
	// provider (tests only; empty derives it from the host).
	ForgeAPI string
	// LLM holds configured AI providers; nil disables AI tasks.
	LLM     *llm.Registry
	Version string
	// JobRoles are the job roles this process runs (ingest, ai, fix): nil
	// runs all of them; an empty slice runs none and only records jobs for
	// workers (docs/SAAS.md, 5e).
	JobRoles []string
	// Worker runs jobs only: no HTTP routes and none of the API's
	// background loops (scheduler, pruning, projections, memory workers).
	Worker bool
}

// Server is the API handler.
type Server struct {
	annotations *annotation.Worker
	cfg         Config
	st          store.Store
	jobs        *Jobs
	// offload extracts memory triples from conversation turns (local model only).
	offload *offload.Worker
	recall  *recall.Service
	sched   *schedules
	orgs    *orgs
	logger  *slog.Logger
	mux     *http.ServeMux
	// classes records every registered pattern's class for governance-only
	// accounts (root.go).
	classes map[string]routeClass
	// cloneHook replaces the clone job for organisation repositories (tests).
	cloneHook func(remote, name string) (Job, error)
}

// New builds the API. Jobs run on ctx and stop when it is cancelled.
func New(ctx context.Context, cfg Config, st store.Store, logger *slog.Logger) *Server {
	s := &Server{cfg: cfg, st: st, logger: logger, mux: http.NewServeMux()}
	s.jobs = NewJobs(ctx, st, logger, cfg.CommitLimit, cfg.AllowNetwork, cfg.VulnScan)
	kinds, err := kindsFor(cfg.JobRoles)
	if err != nil {
		logger.Error("job roles", "error", err)
		kinds = []string{}
	}
	if cfg.Accounts != nil {
		// Jobs are rows before they run and survive restarts (jobs_durable.go).
		s.jobs.UseStore(cfg.Accounts.St, kinds)
	}
	s.jobs.licenseLookup = cfg.LicenseLookup
	s.jobs.dataflow = cfg.Dataflow
	s.jobs.Policy = s.approveIngestPolicy
	s.jobs.Publish = s.publishRepo
	s.jobs.Ingested = s.annotateIngest
	s.jobs.Report = s.saveIngestReport
	s.annotations = annotation.New(cfg.LLM, s.persistAnnotations)
	go s.annotations.Run(ctx)
	if cfg.Accounts != nil {
		if cfg.AuditRetention <= 0 {
			s.cfg.AuditRetention = accounts.DefaultRetention
		}
		if cfg.Accounts.Sink == nil {
			cfg.Accounts.Sink = s.logAudit
		}
		if !cfg.Worker {
			// exportIngestMetrics is fenced across replicas on its own.
			go s.exportIngestMetrics(ctx)
		}
	}
	s.offload = offload.New(st, cfg.LLM, logger)
	s.recall = recall.New(st, cfg.LLM, logger)
	s.offload.OnSessionSaved = s.indexSession
	if !cfg.Worker {
		go s.offload.Run(ctx)
		go s.observeMemoryFacts(ctx)
	}
	sched, err := loadSchedules(cfg.Workspace)
	if err != nil {
		logger.Error("auto-sync schedules unreadable; starting with none", "error", err)
		sched, _ = loadSchedules("") //nolint:errcheck // in-memory fallback cannot fail
	}
	s.sched = sched
	if cfg.Accounts != nil {
		// Shared by every process (schedule.go); a file from before moves in once.
		s.sched.useStore(ctx, cfg.Accounts.St, !cfg.Worker, logger)
	}
	s.orgs, err = loadOrgs(cfg.Workspace)
	if err != nil {
		logger.Error("watched organisations unreadable; starting with none", "error", err)
		s.orgs, _ = loadOrgs("") //nolint:errcheck // in-memory fallback cannot fail
	}
	s.loadRoutes()
	if !cfg.Worker {
		// One API process at a time runs these (singleton.go); workers only
		// run jobs (docs/SAAS.md, 5e).
		loops := []func(context.Context){s.recoverFixes, func(ctx context.Context) { s.runScheduler(ctx, logger) }}
		if cfg.Accounts != nil {
			loops = append(loops, s.pruneAudit, s.projectAccess, s.pruneExecutionMemory, s.recoverIngestReports)
		}
		s.singleton(ctx, loops...)
	}
	if s.jobs.store != nil && len(kinds) > 0 {
		go s.runQueue(ctx)
	}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	r, span := s.traceRequest(r)
	rid := requestID(r)
	if id := telemetry.TraceID(r.Context()); id != "" {
		rid = id // one ID for the audit entry, the logs and the trace
	}
	w.Header().Set("X-Request-ID", rid)
	r = r.WithContext(accounts.WithRequestInfo(r.Context(), accounts.RequestInfo{RequestID: rid, IP: s.clientAddr(r), UserAgent: r.UserAgent()}))
	rec := &statusRecorder{ResponseWriter: w}
	start, tenant := time.Now(), new(string)
	r = r.WithContext(context.WithValue(r.Context(), tenantSlot{}, tenant))
	defer func() {
		s.observe(r, rec.status, *tenant, start)
		_, pattern := s.mux.Handler(r)
		endRequestSpan(span, r.Method, pattern, rec.status)
	}()
	if s.cfg.Accounts != nil {
		var ok bool
		if r, ok = s.authenticate(rec, r); !ok {
			s.auditDenied(r, rec.status)
			return
		}
	}
	s.mux.ServeHTTP(rec, r)
	if mutating(r.Method) || rec.status == http.StatusForbidden || rec.status == http.StatusNotFound {
		_, pattern := s.mux.Handler(r)
		s.auditRequest(r, pattern, rec.status)
	}
}

// tenantSlot lets guard report the request's tenant to ServeHTTP, which
// records the request metrics after the handler ran.
type tenantSlot struct{}

func (s *Server) observe(r *http.Request, status int, tenant string, start time.Time) {
	_, route := s.mux.Handler(r)
	if route == "" {
		route = "(no route)"
	}
	if status == 0 {
		status = http.StatusOK
	}
	if tenant == "" {
		tenant = telemetry.NoTenant
	}
	ctx := telemetry.WithTenant(r.Context(), tenant)
	telemetry.HTTPRequests.IncContext(ctx, tenant, route, r.Method, fmt.Sprintf("%dxx", status/100))
	telemetry.HTTPDuration.ObserveContext(ctx, time.Since(start).Seconds(), route, r.Method)
}

func (s *Server) routes() {
	s.route("GET /healthz", governance(anyone), func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	s.route("GET /api/v1/version", governance(anyone), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]string{"version": s.cfg.Version})
	})
	// Settings report what is configured, never full secret values.
	s.route("GET /api/v1/settings", governance(anyone), func(w http.ResponseWriter, r *http.Request) {
		tokens, tokenHost := s.gitTokens(), s.cfg.GitAuth.Host
		// The installation's own tokens are the operator's business:
		// organisation users manage theirs under their organisation.
		if p, ok := principal(r.Context()); s.cfg.Accounts != nil && (!ok || !p.User.PlatformAdmin) {
			tokens, tokenHost = []gitToken{}, ""
		}
		hosts := make([]string, len(tokens))
		for i, t := range tokens {
			hosts[i] = t.Host
		}
		writeJSON(w, 200, map[string]any{
			"git_token_configured": len(tokens) > 0,
			"git_token_host":       tokenHost,
			"git_token_hosts":      hosts,
			"git_tokens":           tokens,
			"clone_enabled":        s.cfg.Workspace != "",
			"mcp_url":              s.cfg.PublicMCPURL,
			"allow_private_git":    s.cfg.AllowPrivateGit,
			"vuln_scan":            s.cfg.VulnScan,
			"license_lookup":       s.cfg.LicenseLookup,
			"push_enabled":         s.cfg.AllowPush && s.cfg.LLM != nil,
			"push_allowed":         s.cfg.AllowPush,
			"sources_root":         s.cfg.SourcesRoot != "",
		})
	})
	s.route("GET /api/v1/repos", visibleOnly, s.handleRepos)
	s.accountRoutes()
	s.executionRoutes()
	s.importRoutes()
	s.sessionMemoryRoutes()
	s.repoPolicyRoutes()
	s.runtimeRoutes()
	s.retrievalRoutes()
	s.route("GET /api/v1/overview", visibleOnly, s.handleWorkspaceOverview)
	s.route("GET /api/v1/attention", visibleOnly, s.handleAttention)
	s.route("GET /api/v1/stats", visibleOnly, s.handleStats)
	s.route("GET /api/v1/stats/series", governance(anyone), s.handleSeries)
	s.route("DELETE /api/v1/repos/{repo}", repoRole(accounts.Admin), s.handleDeleteRepo)
	s.route("GET /api/v1/repos/{repo}/overview", repoRole(accounts.Viewer), s.handleOverview)
	s.route("GET /api/v1/repos/{repo}/dashboard", repoRole(accounts.Viewer), s.handleDashboard)
	s.route("GET /api/v1/repos/{repo}/sections", repoRole(accounts.Viewer), s.handleSections)
	s.route("GET /api/v1/repos/{repo}/metrics", repoRole(accounts.Viewer), s.handleMetrics)
	s.route("GET /api/v1/repos/{repo}/security", repoRole(accounts.Viewer), s.handleSecurity)
	s.route("GET /api/v1/repos/{repo}/cleanup", repoRole(accounts.Viewer), s.handleCleanup)
	s.route("GET /api/v1/repos/{repo}/duplicates", repoRole(accounts.Viewer), s.handleDuplicates)
	s.route("GET /api/v1/shared-code", visibleOnly, s.handleSharedCode)
	s.route("GET /api/v1/repos/{repo}/sbom", repoRole(accounts.Viewer), s.handleSBOM)
	s.route("GET /api/v1/repos/{repo}/dependencies", repoRole(accounts.Viewer), s.handleDependencies)
	s.route("GET /api/v1/repos/{repo}/duplicates/refactor", repoRole(accounts.Viewer), s.handleRefactorPlan)
	s.route("GET /api/v1/repos/{repo}/architecture", repoRole(accounts.Viewer), s.handleArchitecture)
	s.route("GET /api/v1/repos/{repo}/architecture/record", repoRole(accounts.Viewer), s.handleArchitectureRecord)
	s.route("GET /api/v1/repos/{repo}/architecture/issues", repoRole(accounts.Viewer), s.handleArchitectureIssues)
	s.route("GET /api/v1/repos/{repo}/architecture/context", repoRole(accounts.Viewer), s.handleArchitectureContext)
	s.route("GET /api/v1/repos/{repo}/architecture/proposals", repoRole(accounts.Viewer), s.handleProposals)
	s.route("POST /api/v1/repos/{repo}/architecture/proposals", repoRole(accounts.Member), s.handlePropose)
	s.route("GET /api/v1/repos/{repo}/architecture/proposals/{id}", repoRole(accounts.Viewer), s.handleProposal)
	s.route("POST /api/v1/repos/{repo}/architecture/proposals/{id}/withdraw", repoRole(accounts.Member), s.handleWithdrawProposal)
	s.route("GET /api/v1/repos/{repo}/architecture/proposals/{id}/export", repoRole(accounts.Viewer), s.handleExportProposal)
	s.route("GET /api/v1/repos/{repo}/architecture/links", repoRole(accounts.Viewer), s.handleLinks)
	s.route("POST /api/v1/repos/{repo}/architecture/links", repoRole(accounts.Member), s.handleAddLink)
	s.route("DELETE /api/v1/repos/{repo}/architecture/links/{id}", repoRole(accounts.Member), s.handleRemoveLink)
	s.route("GET /api/v1/repos/{repo}/history", repoRole(accounts.Viewer), s.handleHistory)
	s.route("GET /api/v1/repos/{repo}/cleanup/plan", repoRole(accounts.Viewer), s.handleCleanupPlan)
	s.route("GET /api/v1/repos/{repo}/licenses", repoRole(accounts.Viewer), s.handleLicenses)
	s.route("GET /api/v1/repos/{repo}/memory", repoRole(accounts.Viewer), s.handleMemory)
	s.route("DELETE /api/v1/repos/{repo}/memory", repoRole(accounts.Member), s.withExecution(s.handleDeleteMemory))
	s.route("POST /api/v1/repos/{repo}/memory/offload", repoRole(accounts.Member), s.withExecution(s.handleOffload))
	s.route("POST /api/v1/repos/{repo}/memory/recall", repoRole(accounts.Viewer), s.withExecution(s.handleRecall))
	s.route("GET /api/v1/memory/status", anyone, s.handleMemoryStatus)
	s.route("GET /api/v1/memory/jobs", visibleOnly, s.withExecution(s.handleOffloadJobs))
	s.route("GET /api/v1/memory/jobs/{id}", visibleOnly, s.withExecution(s.handleOffloadJob))
	s.route("GET /api/v1/repos/{repo}/search", repoRole(accounts.Viewer), s.handleSearch)
	s.route("GET /api/v1/repos/{repo}/symbol", repoRole(accounts.Viewer), s.handleSymbol)
	s.route("GET /api/v1/repos/{repo}/identities", repoRole(accounts.Viewer), s.handleIdentities)
	s.route("GET /api/v1/repos/{repo}/code-records", repoRole(accounts.Viewer), s.handleCodeRecords)
	s.route("POST /api/v1/repos/{repo}/code-records", repoRole(accounts.Member), s.handleRecordCode)
	s.route("GET /api/v1/repos/{repo}/outline", repoRole(accounts.Viewer), s.handleOutline)
	s.route("GET /api/v1/repos/{repo}/list", repoRole(accounts.Viewer), s.handleList)
	s.route("GET /api/v1/repos/{repo}/hotspots", repoRole(accounts.Viewer), s.handleHotspots)
	s.route("GET /api/v1/repos/{repo}/graph", repoRole(accounts.Viewer), s.handleGraph)
	s.route("GET /api/v1/repos/{repo}/packages/graph", repoRole(accounts.Viewer), s.handlePackageGraph)
	s.route("GET /api/v1/repos/{repo}/graph/overview", repoRole(accounts.Viewer), s.handleGraphOverview)
	s.route("GET /api/v1/repos/{repo}/graph/expand", repoRole(accounts.Viewer), s.handleGraphExpand)
	s.route("POST /api/v1/repos/{repo}/graph/edges", repoRole(accounts.Viewer), s.handleGraphEdges)
	s.route("POST /api/v1/repos/{repo}/impact", repoRole(accounts.Viewer), s.handleImpact)
	s.route("GET /api/v1/sources", localOnly, s.handleSources)
	s.route("POST /api/v1/ingest", localOnly, s.handleIngest)
	s.route("POST /api/v1/repos", orgRole(accounts.Member), s.withExecution(s.handleRegister))
	s.route("GET /api/v1/orgs", localOnly, s.handleListOrgs)
	s.route("POST /api/v1/orgs", localOnly, s.handleAddOrg)
	s.route("PUT /api/v1/orgs/{org}", localOnly, s.handleUpdateOrg)
	s.route("DELETE /api/v1/orgs/{org}", localOnly, s.handleDeleteOrg)
	s.route("POST /api/v1/orgs/{org}/scan", localOnly, s.handleScanOrg)
	s.route("POST /api/v1/orgs/{org}/repos/{name}/index", localOnly, s.handleIndexOrgRepo)
	s.route("POST /api/v1/repos/{repo}/sync", repoRole(accounts.Admin), s.withExecution(s.handleSync))
	s.route("GET /api/v1/repos/{repo}/schedule", repoRole(accounts.Viewer), s.handleGetSchedule)
	s.route("PUT /api/v1/repos/{repo}/schedule", repoRole(accounts.Admin), s.handlePutSchedule)
	s.route("GET /api/v1/repos/{repo}/branches", repoRole(accounts.Viewer), s.handleBranches)
	s.route("GET /api/v1/rules", governance(anyone), func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"rules": security.Catalog()})
	})
	s.route("GET /api/v1/repos/{repo}/reviews", repoRole(accounts.Viewer), s.handleListReviews)
	s.route("GET /api/v1/reviews/{id}", reviewRole(accounts.Viewer), s.handleGetReview)
	s.route("GET /api/v1/reviews/{id}/code", reviewRole(accounts.Viewer), s.handleReviewCode)
	s.route("DELETE /api/v1/reviews/{id}", reviewRole(accounts.Admin), s.handleDeleteReview)
	s.route("PATCH /api/v1/reviews/{id}/comments/{comment}", reviewRole(accounts.Member), s.handleUpdateReviewComment)
	s.route("POST /api/v1/reviews/{id}/fix", reviewRole(accounts.Member), s.withExecution(s.handleFixReview))
	s.route("POST /api/v1/repos/{repo}/remote", repoRole(accounts.Admin), s.withExecution(s.handleSwitchToRemote))
	s.route("GET /api/v1/ai/providers", governance(anyone), s.handleAIProviders)
	s.route("PUT /api/v1/ai/routing", governance(platformOnly), s.handlePutRouting)
	s.route("POST /api/v1/ai/providers/{name}/test", governance(platformOnly), s.withExecution(s.handleAITest))
	s.route("POST /api/v1/repos/{repo}/ai/{task}", repoRole(accounts.Member), s.withExecution(s.handleAITask))
	s.route("GET /api/v1/jobs", visibleOnly, s.handleJobs)
	s.route("GET /api/v1/jobs/{id}", visibleOnly, s.handleJob)
}

// problem writes an RFC 7807 response. Internal errors are logged and
// replaced with a generic message so driver errors do not leak to clients.
func (s *Server) problem(w http.ResponseWriter, status int, err error) {
	detail := err.Error()
	var amb *query.AmbiguousError
	switch {
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.As(err, &amb):
		status = http.StatusConflict
	case status >= 500:
		s.logger.Error("request failed", "error", err)
		detail = "internal error"
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // client went away
		"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // client went away
}

func intParam(r *http.Request, name string, def int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || v <= 0 {
		return def
	}
	return v
}

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.st.Repos(r.Context())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	repos = visibleRepos(r.Context(), repos)
	out := make([]map[string]any, 0, len(repos))
	for _, n := range repos {
		out = append(out, map[string]any{
			"name": n.Name, "head": n.Head, "ingested_at": n.Time, "root": s.displayRoot(n.Root),
			"branch": n.Branch, "remote": n.URL, "managed": s.inWorkspace(n.Root),
		})
	}
	writeJSON(w, 200, map[string]any{"repos": out})
}

// displayRoot shows roots relative to the sources root, hiding host layout.
func (s *Server) displayRoot(root string) string {
	if s.inWorkspace(root) {
		if rel, err := filepath.Rel(s.cfg.Workspace, root); err == nil {
			return filepath.ToSlash(rel) // owner/name for managed clones
		}
	}
	if s.cfg.SourcesRoot == "" || root == "" {
		return filepath.Base(root)
	}
	if rel, err := filepath.Rel(s.cfg.SourcesRoot, root); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return filepath.Base(root)
}

// handleDeleteRepo removes a repository's graph and auto-sync schedule.
// With ?files=1 it also deletes a clone in the workspace; mounted sources
// are never touched.
func (s *Server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if s.cfg.Accounts != nil {
		// An organisation removes its link; the graph goes only with the
		// last link.
		left, err := s.unlinkRepo(r, repo)
		if err != nil {
			s.accountError(w, err)
			return
		}
		if left > 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	status, err := s.deleteRepo(r.Context(), repo, r.URL.Query().Get("files") == "1")
	if err != nil {
		s.problem(w, status, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// unlinkRepo removes the links the request may remove and returns how
// many organisations still use the graph.
func (s *Server) unlinkRepo(r *http.Request, graph string) (int, error) {
	ctx := r.Context()
	sc := scopeFrom(ctx)
	links, err := s.cfg.Accounts.St.GraphRepositories(ctx, graph)
	if err != nil {
		return 0, err
	}
	left := len(links)
	for _, l := range links {
		reach, ok := sc.reach[graph]
		mine := ok && !reach.Ambiguous && reach.OrgID == l.OrgID && reach.Role.AtLeast(accounts.Admin)
		if !mine {
			continue
		}
		if left, err = s.cfg.Accounts.St.RemoveRepository(ctx, l.OrgID, graph); err != nil {
			return 0, err
		}
		if err := s.cfg.Accounts.DropRepoGrants(ctx, l.OrgID, graph); err != nil {
			return 0, err
		}
		// The organisation's uploaded test runs leave with its link.
		if rs, ok := s.st.(store.RuntimeStore); ok {
			if err := rs.DeleteRuntimeFacts(ctx, memory.Namespace(l.OrgID, graph)); err != nil {
				return 0, err
			}
		}
	}
	return left, nil
}

// deleteRepo removes a repository's graph and schedule and, when files is
// set, its clone. On failure it returns the HTTP status to report.
func (s *Server) deleteRepo(ctx context.Context, repo string, files bool) (int, error) {
	for _, j := range s.jobs.List(ctx) {
		if j.Repo == repo && j.State == JobRunning {
			return http.StatusConflict, fmt.Errorf("%s is being indexed; try again when the job finishes", repo)
		}
	}
	n, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return 500, err
	}
	if files && s.inWorkspace(n.Root) {
		rel, relErr := filepath.Rel(s.cfg.Workspace, n.Root)
		if relErr != nil || rel == "." || strings.Contains(rel, "..") || strings.Count(rel, string(filepath.Separator)) > 2 {
			return 400, errors.New("refusing to delete files outside a single workspace clone")
		}
		if err := os.RemoveAll(n.Root); err != nil {
			return 500, err
		}
		// os.Remove only deletes an empty folder: the owner (and host) folder goes
		// when this was its last clone.
		for dir := filepath.Dir(n.Root); dir != filepath.Clean(s.cfg.Workspace) && strings.HasPrefix(dir, filepath.Clean(s.cfg.Workspace)+string(filepath.Separator)); dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil { //nolint:errcheck,gosec // non-empty is the normal case; dir is inside the workspace
				break
			}
		}
	}
	if err := s.st.DeleteRepo(ctx, repo); err != nil {
		return 500, err
	}
	if err := s.st.DeleteSnapshots(ctx, repo); err != nil {
		s.logger.Warn("removing history", "repo", repo, "error", err)
	}
	if err := s.st.DeleteAllCurated(ctx, repo); err != nil {
		s.logger.Warn("removing proposals and links", "repo", repo, "error", err)
	}
	if err := s.sched.remove(ctx, repo); err != nil {
		s.logger.Warn("removing schedule", "repo", repo, "error", err)
	}
	return http.StatusNoContent, nil
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.repoQuery(r.PathValue("repo")).Overview(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	ov.Repo.Root = s.displayRoot(ov.Repo.Root)
	writeJSON(w, 200, ov)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.problem(w, 400, errors.New("q is required"))
		return
	}
	kinds, err := parseKinds(r.URL.Query()["kind"])
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	ns, err := s.st.Search(r.Context(), r.PathValue("repo"), q, kinds, intParam(r, "limit", 25))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	refs := make([]query.Ref, 0, len(ns))
	for _, n := range ns {
		refs = append(refs, query.RefOf(n))
	}
	writeJSON(w, 200, map[string]any{"results": refs})
}

func parseKinds(raw []string) ([]model.NodeKind, error) {
	var out []model.NodeKind
	for _, k := range raw {
		for _, part := range strings.Split(k, ",") {
			nk := model.NodeKind(strings.TrimSpace(part))
			if nk == "" {
				continue
			}
			if !nk.Valid() {
				return nil, fmt.Errorf("unknown kind %q", nk)
			}
			out = append(out, nk)
		}
	}
	return out, nil
}

func (s *Server) handleSymbol(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		s.problem(w, 400, errors.New("id is required"))
		return
	}
	card, err := s.repoQuery(r.PathValue("repo")).Card(r.Context(), r.PathValue("repo"), id, intParam(r, "limit", 20))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, card)
}

func (s *Server) handleOutline(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	if file == "" {
		s.problem(w, 400, errors.New("file is required"))
		return
	}
	entries, err := s.repoQuery(r.PathValue("repo")).Outline(r.Context(), r.PathValue("repo"), file)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"file": file, "entries": entries})
}

// handleList is the generic table endpoint behind most dashboard views:
// ?kind=Function&metric=complexity&tag=exec&prefix=internal/&exported=1&undocumented=1&notests=1
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	kinds, err := parseKinds(qv["kind"])
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	m := store.Metric(qv.Get("metric"))
	if m == "" {
		m = store.ByName
	}
	if !m.Valid() {
		s.problem(w, 400, fmt.Errorf("unknown metric %q", m))
		return
	}
	f := store.Filter{
		Kinds: kinds, Tag: qv.Get("tag"), FilePrefix: qv.Get("prefix"),
		ExportedOnly: qv.Get("exported") == "1", Undocumented: qv.Get("undocumented") == "1",
		ExcludeTests: qv.Get("notests") == "1",
	}
	ranked, err := s.st.List(r.Context(), r.PathValue("repo"), f, m, intParam(r, "limit", 50))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if ranked == nil {
		ranked = []store.Ranked{}
	}
	writeJSON(w, 200, map[string]any{"results": ranked})
}

func (s *Server) handleHotspots(w http.ResponseWriter, r *http.Request) {
	hs, err := s.repoQuery(r.PathValue("repo")).Hotspots(r.Context(), r.PathValue("repo"), intParam(r, "limit", 25))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"hotspots": hs})
}

// GraphView is a node-link payload for the UI's graph canvas.
type GraphView struct {
	Nodes []GraphNode  `json:"nodes"`
	Edges []model.Edge `json:"edges"`
	Root  string       `json:"root,omitempty"`
}

// GraphNode is a node in a GraphView.
type GraphNode struct {
	query.Ref
	Complexity int      `json:"complexity,omitempty"`
	Tags       []string `json:"security_tags,omitempty"`
	IsTest     bool     `json:"is_test,omitempty"`
}

func graphNode(n model.Node, depth int) GraphNode {
	ref := query.RefOf(n)
	ref.Depth = depth
	return GraphNode{Ref: ref, Complexity: n.Complexity, Tags: n.SecurityTags, IsTest: n.IsTest}
}

// handleGraph returns the call neighbourhood of a symbol: callers and
// callees up to depth, with the edges between them.
func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	root, err := s.repoQuery(r.PathValue("repo")).Resolve(ctx, r.PathValue("repo"), r.URL.Query().Get("id"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	depth := min(intParam(r, "depth", 2), 3)
	limit := min(intParam(r, "limit", 80), 200)
	edges := []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}
	in, err := store.ForRepository(s.st, r.PathValue("repo")).Traverse(ctx, root.ID, edges, store.In, depth, limit)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out, err := store.ForRepository(s.st, r.PathValue("repo")).Traverse(ctx, root.ID, edges, store.Out, depth, limit)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	view := GraphView{Root: root.ID, Nodes: []GraphNode{graphNode(root, 0)}}
	ids := []string{root.ID}
	seen := map[string]bool{root.ID: true}
	for _, h := range slices.Concat(in, out) {
		if seen[h.Node.ID] {
			continue
		}
		seen[h.Node.ID] = true
		ids = append(ids, h.Node.ID)
		view.Nodes = append(view.Nodes, graphNode(h.Node, h.Depth))
	}
	view.Edges, err = store.ForRepository(s.st, r.PathValue("repo")).Subgraph(ctx, ids, edges)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, view)
}

func (s *Server) handlePackageGraph(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pkgs, err := s.st.List(ctx, r.PathValue("repo"), store.Filter{Kinds: []model.NodeKind{model.KindPackage}}, store.ByName, store.MaxLimit)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	view := GraphView{Nodes: make([]GraphNode, 0, len(pkgs))}
	ids := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		ids = append(ids, p.Node.ID)
		view.Nodes = append(view.Nodes, graphNode(p.Node, 0))
	}
	view.Edges, err = store.ForRepository(s.st, r.PathValue("repo")).Subgraph(ctx, ids, []model.EdgeKind{model.EdgeImports})
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, view)
}

type impactRequest struct {
	Diff  string `json:"diff"`
	Base  string `json:"base"`
	Head  string `json:"head"`
	Depth int    `json:"depth"`
}

func (s *Server) handleImpact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo := r.PathValue("repo")
	var req impactRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	diff := []byte(req.Diff)
	if len(diff) == 0 {
		if req.Base == "" {
			s.problem(w, 400, errors.New("diff or base is required"))
			return
		}
		repoNode, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		if err := s.insideSources(repoNode.Root); err != nil {
			s.problem(w, 400, err)
			return
		}
		head := req.Head
		if head == "" {
			head = "HEAD"
		}
		diff, err = gitx.Diff(ctx, repoNode.Root, req.Base, head)
		if err != nil {
			status := 500
			if errors.Is(err, gitx.ErrInvalidRev) {
				status = 400
			}
			s.problem(w, status, err)
			return
		}
	}
	changes, err := gitx.ParseDiff(diff)
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	rep, err := review.Analyze(ctx, s.repoQuery(repo), repo, changes, review.Options{Depth: req.Depth})
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if r.URL.Query().Get("format") == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_ = review.WriteMarkdown(w, rep) //nolint:errcheck // client went away
		return
	}
	writeJSON(w, 200, rep)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return errors.New("content-type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid json body: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single json object")
	}
	return nil
}

// Source is a directory under the sources root that can be ingested.
type Source struct {
	Name  string `json:"name"`
	IsGit bool   `json:"is_git"`
	IsGo  bool   `json:"is_go"`
	// Content counts files per detected format (go, python, typescript,
	// yaml, helm, markdown, ...) from a bounded scan.
	Content map[string]int `json:"content"`
	// Truncated is set when the scan stopped before the end of the tree.
	Truncated bool `json:"truncated,omitempty"`
}

func (s *Server) handleSources(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.SourcesRoot == "" {
		writeJSON(w, 200, map[string]any{"sources": []Source{}})
		return
	}
	entries, err := os.ReadDir(s.cfg.SourcesRoot)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := []Source{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(s.cfg.SourcesRoot, e.Name())
		content, truncated := scanContent(dir)
		out = append(out, Source{Name: e.Name(), IsGit: exists(filepath.Join(dir, ".git")), IsGo: content["go"] > 0, Content: content, Truncated: truncated})
	}
	writeJSON(w, 200, map[string]any{"sources": out})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// insideSources rejects paths that resolve (after symlinks) outside the
// sources root.
func (s *Server) insideSources(path string) error {
	if s.cfg.SourcesRoot == "" {
		return errors.New("no sources root configured")
	}
	root, err := filepath.EvalSymlinks(s.cfg.SourcesRoot)
	if err != nil {
		return fmt.Errorf("sources root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return errors.New("path does not exist")
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path is outside the sources root")
	}
	return nil
}

type ingestRequest struct {
	// Source is a directory name under the sources root.
	Source string `json:"source"`
	// Repo is the graph name; defaults to Source.
	Repo string `json:"repo"`
}

// authFor returns the token configured for remote's host, if any.
func (s *Server) authFor(remote string) gitx.Auth {
	return gitx.AuthFor(remote, append([]gitx.Auth{s.cfg.GitAuth}, s.cfg.GitAuths...)...)
}

// gitToken is what the settings endpoint shows about a configured token.
type gitToken struct {
	Host    string `json:"host"`
	Preview string `json:"preview"`
}

// gitTokens lists one entry per host, in the order authFor picks them, with
// the token masked.
func (s *Server) gitTokens() []gitToken {
	out := []gitToken{}
	for _, a := range append([]gitx.Auth{s.cfg.GitAuth}, s.cfg.GitAuths...) {
		host := strings.ToLower(a.Host)
		if a.Token == "" || host == "" || slices.ContainsFunc(out, func(t gitToken) bool { return t.Host == host }) {
			continue
		}
		out = append(out, gitToken{Host: host, Preview: maskToken(a.Token)})
	}
	return out
}

// maskToken keeps the first and last five characters so a user can tell which
// token is loaded. Short tokens are hidden completely: five plus five would
// give away most of them.
func maskToken(t string) string {
	if len(t) < 20 {
		return "***"
	}
	return t[:5] + "***" + t[len(t)-5:]
}

// validRepoName restricts repo names to a safe, URL-friendly alphabet: "name",
// "owner/name", or "host/owner/name" for hosts other than github.com.
func validRepoName(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) > 3 {
		return false
	}
	for _, p := range parts {
		if !validRepoSegment(p) {
			return false
		}
	}
	return true
}

// validRepoSegment restricts one path element of a repository name to a
// safe, URL-friendly alphabet.
func validRepoSegment(name string) bool {
	// A leading dot would allow "." and "..", and collide with the
	// workspace's .codesynapse state directory.
	if name == "" || len(name) > 100 || strings.HasPrefix(name, ".") {
		return false
	}
	for _, c := range name {
		ok := c == '-' || c == '_' || c == '.' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// repoDir is where a managed clone lives: <workspace>/<name>, so
// "astrona-io/astrona-cli" is <workspace>/astrona-io/astrona-cli and
// "codeberg.org/team/api" is <workspace>/codeberg.org/team/api. The name must
// have passed validRepoName: at most three safe elements, none starting with
// a dot.
func repoDir(workspace, name string) string {
	return filepath.Join(workspace, filepath.FromSlash(name))
}

// repoNameFromRemote names a repository after its remote (forge.NameFromRemote),
// or "" when the result is not a valid repository name here, for instance a
// GitLab subgroup path the name cannot hold.
func repoNameFromRemote(remote string) string {
	name := forge.NameFromRemote(remote)
	if !validRepoName(name) {
		return ""
	}
	return name
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if req.Repo == "" {
		req.Repo = req.Source
	}
	if !validRepoName(req.Source) || !validRepoName(req.Repo) {
		s.problem(w, 400, errors.New("source and repo must match [A-Za-z0-9._-]+"))
		return
	}
	path := filepath.Join(s.cfg.SourcesRoot, req.Source)
	if err := s.insideSources(path); err != nil {
		s.problem(w, 400, err)
		return
	}
	org, authorize := s.jobAuthorization(r.Context(), req.Repo, accounts.Admin)
	job, err := s.jobs.Start(req.Repo, path, JobOptions{Identity: identityOf(r.Context()), OrgID: org, Authorize: authorize}) //nolint:contextcheck // jobs outlive the request by design
	if err != nil {
		s.problem(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	sc := scopeFrom(r.Context())
	jobs := slices.DeleteFunc(s.jobs.List(r.Context()), func(j Job) bool { return !seesReview(sc, j.Repo, j.OrgID) })
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobs.Get(r.Context(), r.PathValue("id"))
	if !ok || !seesReview(scopeFrom(r.Context()), job.Repo, job.OrgID) {
		s.problem(w, 404, store.ErrNotFound)
		return
	}
	writeJSON(w, 200, job)
}

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	minSev := r.URL.Query().Get("min")
	if minSev != "" && model.SeverityRank(minSev) < 0 {
		s.problem(w, 400, fmt.Errorf("unknown severity %q", minSev))
		return
	}
	rep, err := security.Build(r.Context(), store.ForRepository(s.st, r.PathValue("repo")), r.PathValue("repo"), s.repositorySecurityOptions(r.Context(), r.PathValue("repo"), minSev))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if r.URL.Query().Get("format") == "markdown" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_ = security.WriteMarkdown(w, rep) //nolint:errcheck // client went away
		return
	}
	writeJSON(w, 200, rep)
}
