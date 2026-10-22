// Command codesynapse-mcp serves the code graph to AI agents over MCP, on
// stdio (for a local Claude Code / IDE client) or streamable HTTP (as a
// docker compose service).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	"github.com/parisnakitakejser/codesynapse/internal/accounts/pgstore"
	"github.com/parisnakitakejser/codesynapse/internal/app"
	"github.com/parisnakitakejser/codesynapse/internal/ingest"
	"github.com/parisnakitakejser/codesynapse/internal/logx"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/usage"
	"github.com/parisnakitakejser/codesynapse/services/mcp/internal/mcpserver"
)

var version = "dev"

// EnvToken is the bearer token required by the HTTP transport.
const EnvToken = "CODESYNAPSE_MCP_TOKEN" //nolint:gosec // env var name

func main() {
	os.Exit(app.Main("codesynapse-mcp", "MCP server exposing the code graph to AI agents", version,
		func(ec *app.Env) []*cobra.Command { return []*cobra.Command{serveCmd(ec)} }))
}

type serveOptions struct {
	EC        *app.Env
	neo4j     app.Neo4jFlags
	transport string
	addr      string
	offline   string
	reviews   bool
	memory    bool
	usage     bool
	apiURL    string
}

func (o *serveOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.neo4j.AddFlags(f)
	f.StringVar(&o.transport, "transport", app.Getenv("CODESYNAPSE_MCP_TRANSPORT", "stdio"), "stdio or http")
	f.StringVar(&o.addr, "addr", app.Getenv("CODESYNAPSE_MCP_ADDR", "127.0.0.1:8090"), "Listen address for the http transport")
	f.StringVar(&o.offline, "offline", "", "Serve an in-memory graph of this directory instead of Neo4j")
	f.BoolVar(&o.memory, "allow-memory", app.Getenv("CODESYNAPSE_ALLOW_MEMORY", "true") == "true", "Enable the agent-memory tools upsert_graph_triples, search_graph_nodes and query_subgraph (writes only to the memory store)")
	f.StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", ""), "CodeSynapse API base URL; enables offload_turns (background triple extraction by the API's local model). Token from CODESYNAPSE_API_TOKEN")
	f.BoolVar(&o.usage, "record-usage", app.Getenv("CODESYNAPSE_MCP_USAGE", "true") == "true", "Count sessions and tool calls per day, repository, tool and client for the Statistics page (never arguments or results)")
	f.BoolVar(&o.reviews, "allow-reviews", app.Getenv("CODESYNAPSE_MCP_REVIEWS", "true") == "true", "Enable submit_review/update_review_comment (writes only to the review store)")
	return nil
}

func (o *serveOptions) Complete(context.Context, *app.Env) error { return nil }

func (o *serveOptions) Validate(_ context.Context, _ *app.Env) error {
	if o.apiURL != "" {
		u, err := url.Parse(o.apiURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("--api-url must be an http(s) URL without credentials, e.g. http://api:8080")
		}
	}
	switch o.transport {
	case "stdio":
		return nil
	case "http":
		// With accounts, personal API keys protect the endpoint.
		if os.Getenv(EnvToken) == "" && os.Getenv(EnvDatabaseURL) == "" && !app.IsLoopback(o.addr) {
			return fmt.Errorf("%w (%s)", app.ErrTokenRequired, EnvToken)
		}
		return nil
	}
	return fmt.Errorf("unknown transport %q", o.transport)
}

func (o *serveOptions) open(ctx context.Context) (store.Reader, func(), error) {
	if o.offline != "" {
		root, err := filepath.Abs(o.offline)
		if err != nil {
			return nil, nil, err
		}
		repo := filepath.Base(root)
		g, err := ingest.Run(ctx, ingest.Options{Repo: repo, Root: root, CommitLimit: 200})
		if err != nil {
			return nil, nil, err
		}
		mem := memory.New()
		if err := mem.ReplaceRepo(ctx, repo, g); err != nil {
			return nil, nil, err
		}
		return mem, func() {}, nil
	}
	st, err := o.neo4j.OpenWithRetry(ctx, func(err error) {
		o.EC.Logger.Warn("waiting for neo4j", "error", err)
	})
	if err != nil {
		return nil, nil, err
	}
	return st, func() { _ = st.Close(context.Background()) }, nil //nolint:errcheck,contextcheck // shutdown
}

func (o *serveOptions) Run(ctx context.Context, _ *app.Env) error {
	ctx = app.Ctx(ctx, o.EC)
	r, closeFn, err := o.open(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	var opts mcpserver.Options
	if rs, ok := r.(store.ReviewStore); ok && o.reviews {
		opts.Reviews = rs
	}
	if ms, ok := r.(store.MemoryStore); ok && o.memory {
		opts.Memory = ms
		if o.apiURL != "" {
			opts.Offload = &mcpserver.APIClient{BaseURL: o.apiURL, Token: os.Getenv("CODESYNAPSE_API_TOKEN")}
		}
	}
	if us, ok := r.(store.UsageStore); ok && o.usage {
		opts.Usage = usage.NewRecorder(us)
		usageCtx, stopUsage := context.WithCancel(ctx)
		flushed := make(chan struct{})
		go func() {
			defer close(flushed)
			opts.Usage.Run(usageCtx, 30*time.Second, func(err error) {
				logx.FromContext(ctx).Warn("recording MCP usage failed", logx.Error(err))
			})
		}()
		// Stop the recorder and wait for its final flush before exiting.
		defer func() { stopUsage(); <-flushed }()
	}
	stopMetrics, err := app.StartMetrics(ctx, o.EC.Logger, "mcp")
	if err != nil {
		return err
	}
	defer stopMetrics()
	acc, closeAccounts, err := o.openAccounts(ctx)
	if err != nil {
		return err
	}
	defer closeAccounts()
	if acc != nil {
		opts.Access = mcpserver.AccountsAccess{Acc: acc}
	}
	server := mcpserver.New(r, version, opts)

	if o.transport == "stdio" {
		// stdout carries the protocol; all logs go to stderr.
		err := server.Run(ctx, &mcp.StdioTransport{})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux := http.NewServeMux()
	if acc != nil {
		// Personal API keys or the MCP token; tools are scoped to the key's organisation.
		mux.Handle("/mcp", auth.RequireBearerToken(mcpserver.KeyVerifier(acc, os.Getenv(EnvToken)),
			&auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler))
	} else {
		mux.Handle("/mcp", app.RequireBearer(os.Getenv(EnvToken), handler))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }) //nolint:errcheck // health probe
	o.EC.Logger.Info("mcp http listening", "addr", o.addr, "path", "/mcp", "auth", os.Getenv(EnvToken) != "" || acc != nil, "accounts", acc != nil)
	return app.Serve(ctx, app.NewServer(o.addr, mux))
}

// EnvDatabaseURL turns on personal API keys. It holds a password, so it is
// read from the environment only. The API owns the schema and migrates it.
const EnvDatabaseURL = "CODESYNAPSE_DATABASE_URL" //nolint:gosec // env var name

// openAccounts connects to the accounts database, or returns nil without one
// (the MCP token alone, every repository).
func (o *serveOptions) openAccounts(ctx context.Context) (*accounts.Service, func(), error) {
	url := os.Getenv(EnvDatabaseURL)
	if url == "" || o.transport != "http" {
		return nil, func() {}, nil
	}
	st, err := pgstore.OpenWait(ctx, url, func(attempt int, err error) {
		o.EC.Logger.Warn("waiting for PostgreSQL", "attempt", attempt, "error", err)
	})
	if err != nil {
		return nil, nil, err
	}
	return accounts.NewService(st, false), st.Close, nil
}

func serveCmd(ec *app.Env) *cobra.Command {
	o := &serveOptions{EC: ec}
	return app.NewSubCommand("serve", o, ec).
		WithShortDesc("Run the MCP server").
		WithExample("  codesynapse-mcp serve                       # stdio, Neo4j\n  codesynapse-mcp serve --offline .           # stdio, no database\n  codesynapse-mcp serve --transport http --addr 0.0.0.0:8090").
		WithGroupID(app.GroupBase).
		WithNoArgs().
		Build()
}
