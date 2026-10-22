// Command codesynapse-mcp serves the code graph to AI agents over MCP, on
// stdio (for a local Claude Code / IDE client) or streamable HTTP (as a
// docker compose service).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/parisnakitakejser/codesynapse/internal/app"
	"github.com/parisnakitakejser/codesynapse/internal/ingest"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
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
}

func (o *serveOptions) SetupFlags(_ context.Context, _ *app.Env) error {
	f := o.EC.Command.Flags()
	o.neo4j.AddFlags(f)
	f.StringVar(&o.transport, "transport", app.Getenv("CODESYNAPSE_MCP_TRANSPORT", "stdio"), "stdio or http")
	f.StringVar(&o.addr, "addr", app.Getenv("CODESYNAPSE_MCP_ADDR", "127.0.0.1:8090"), "Listen address for the http transport")
	f.StringVar(&o.offline, "offline", "", "Serve an in-memory graph of this directory instead of Neo4j")
	f.BoolVar(&o.memory, "allow-memory", app.Getenv("CODESYNAPSE_ALLOW_MEMORY", "") == "true", "Enable the agent-memory tools upsert_graph_triples, search_graph_nodes and query_subgraph (writes only to the memory store)")
	f.BoolVar(&o.reviews, "allow-reviews", app.Getenv("CODESYNAPSE_MCP_REVIEWS", "true") == "true", "Enable submit_review/update_review_comment (writes only to the review store)")
	return nil
}

func (o *serveOptions) Complete(context.Context, *app.Env) error { return nil }

func (o *serveOptions) Validate(_ context.Context, _ *app.Env) error {
	switch o.transport {
	case "stdio":
		return nil
	case "http":
		if os.Getenv(EnvToken) == "" && !app.IsLoopback(o.addr) {
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
	mux.Handle("/mcp", app.RequireBearer(os.Getenv(EnvToken), handler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }) //nolint:errcheck // health probe
	o.EC.Logger.Info("mcp http listening", "addr", o.addr, "path", "/mcp", "auth", os.Getenv(EnvToken) != "")
	return app.Serve(ctx, app.NewServer(o.addr, mux))
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
