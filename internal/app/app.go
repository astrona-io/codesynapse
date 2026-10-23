// Package app holds wiring shared by the codesynapse binaries: Neo4j
// connection flags, store construction and HTTP hardening helpers.
package app

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/hugegraphstore"
	"github.com/astrona-io/codesynapse/internal/store/neo4jstore"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/internal/vector/milvus"
)

// Environment variables. The password is only read from the environment so
// it never appears in process listings or shell history.
const (
	EnvNeo4jURI      = "CODESYNAPSE_NEO4J_URI"
	EnvNeo4jUser     = "CODESYNAPSE_NEO4J_USER"
	EnvNeo4jPassword = "CODESYNAPSE_NEO4J_PASSWORD" //nolint:gosec // env var name, not a credential
	EnvNeo4jDatabase = "CODESYNAPSE_NEO4J_DATABASE"

	// EnvGraphBackend picks the graph database: neo4j (default) or hugegraph.
	EnvGraphBackend      = "CODESYNAPSE_GRAPH_BACKEND"
	EnvHugeGraphURL      = "CODESYNAPSE_HUGEGRAPH_URL"
	EnvHugeGraphGraph    = "CODESYNAPSE_HUGEGRAPH_GRAPH"
	EnvHugeGraphUser     = "CODESYNAPSE_HUGEGRAPH_USER"
	EnvHugeGraphPassword = "CODESYNAPSE_HUGEGRAPH_PASSWORD" //nolint:gosec // env var name, not a credential
	// EnvHugeGraphMaxRequest is the server's Gremlin maxContentLength in
	// bytes (default 60000, under HugeGraph's 65536).
	EnvHugeGraphMaxRequest = "CODESYNAPSE_HUGEGRAPH_MAX_REQUEST_BYTES"
	EnvHugeGraphTxCapacity = "CODESYNAPSE_HUGEGRAPH_TX_CAPACITY"
	// EnvMilvusURL turns on Milvus for agent-memory vectors with HugeGraph.
	EnvMilvusURL   = "CODESYNAPSE_MILVUS_URL"
	EnvMilvusToken = "CODESYNAPSE_MILVUS_TOKEN" //nolint:gosec // env var name, not a credential
)

// Neo4jFlags are the store settings shared by all binaries. Despite the
// name they also select HugeGraph (with Milvus for vectors).
type Neo4jFlags struct {
	URI      string
	User     string
	Database string

	Backend        string
	HugeGraphURL   string
	HugeGraphGraph string
	MilvusURL      string
}

// AddFlags registers the flags on fs with environment-derived defaults.
func (f *Neo4jFlags) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&f.URI, "neo4j-uri", Getenv(EnvNeo4jURI, "neo4j://localhost:7687"), "Neo4j URI (env "+EnvNeo4jURI+")")
	fs.StringVar(&f.User, "neo4j-user", Getenv(EnvNeo4jUser, "neo4j"), "Neo4j user (env "+EnvNeo4jUser+")")
	fs.StringVar(&f.Database, "neo4j-database", Getenv(EnvNeo4jDatabase, "neo4j"), "Neo4j database (env "+EnvNeo4jDatabase+")")
	fs.StringVar(&f.Backend, "graph-backend", Getenv(EnvGraphBackend, "neo4j"), "Graph database: neo4j or hugegraph (env "+EnvGraphBackend+")")
	fs.StringVar(&f.HugeGraphURL, "hugegraph-url", Getenv(EnvHugeGraphURL, "http://localhost:8180"), "HugeGraph server URL (env "+EnvHugeGraphURL+"; user and password from "+EnvHugeGraphUser+"/"+EnvHugeGraphPassword+")")
	fs.StringVar(&f.HugeGraphGraph, "hugegraph-graph", Getenv(EnvHugeGraphGraph, "hugegraph"), "HugeGraph graph name (env "+EnvHugeGraphGraph+")")
	fs.StringVar(&f.MilvusURL, "milvus-url", Getenv(EnvMilvusURL, ""), "Milvus REST URL for agent-memory vectors with HugeGraph, e.g. http://milvus:19530 (env "+EnvMilvusURL+"; token from "+EnvMilvusToken+")")
}

// Open connects to the selected graph database.
func (f *Neo4jFlags) Open(ctx context.Context) (store.Store, error) {
	switch f.Backend {
	case "", "neo4j":
	case "hugegraph":
		return f.openHugeGraph(ctx)
	default:
		return nil, fmt.Errorf("unknown graph backend %q (%s is not set to neo4j or hugegraph)", f.Backend, EnvGraphBackend)
	}
	pw := os.Getenv(EnvNeo4jPassword)
	if pw == "" {
		return nil, fmt.Errorf("%s is not set", EnvNeo4jPassword)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return neo4jstore.New(ctx, neo4jstore.Config{URI: f.URI, User: f.User, Password: pw, Database: f.Database})
}

func (f *Neo4jFlags) openHugeGraph(ctx context.Context) (store.Store, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	maxRequest, err := strconv.Atoi(Getenv(EnvHugeGraphMaxRequest, "0"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvHugeGraphMaxRequest, err)
	}
	txCapacity, err := strconv.Atoi(Getenv(EnvHugeGraphTxCapacity, "0"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvHugeGraphTxCapacity, err)
	}
	var vec vector.Index
	if f.MilvusURL != "" {
		mx, err := milvus.New(ctx, milvus.Config{URL: f.MilvusURL, Token: os.Getenv(EnvMilvusToken)})
		if err != nil {
			return nil, err
		}
		vec = mx
	}
	s, err := hugegraphstore.New(ctx, hugegraphstore.Config{URL: f.HugeGraphURL, Graph: f.HugeGraphGraph,
		User: os.Getenv(EnvHugeGraphUser), Password: os.Getenv(EnvHugeGraphPassword), Vectors: vec, MaxRequestBytes: maxRequest, TxCapacity: txCapacity})
	if err != nil && vec != nil {
		_ = vec.Close(ctx) //nolint:errcheck // the connection error is the one worth returning
	}
	return s, err
}

// OpenWithRetry retries Open until it succeeds or ctx ends. Services use it
// so they can start before Neo4j finishes booting under docker compose.
func (f *Neo4jFlags) OpenWithRetry(ctx context.Context, onRetry func(error)) (store.Store, error) {
	delay := time.Second
	for {
		s, err := f.Open(ctx)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil || strings.Contains(err.Error(), "is not set") {
			return nil, err
		}
		onRetry(err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 15*time.Second)
	}
}

// Getenv returns the environment value of key, or def when unset.
func Getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// IsLoopback reports whether addr ("host:port") binds only to loopback.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ErrTokenRequired is returned when a service would listen on a non-loopback
// address without authentication.
var ErrTokenRequired = errors.New("refusing to listen on a non-loopback address without a token; set one or bind to 127.0.0.1")

// RequireBearer wraps h so every request must carry "Authorization: Bearer
// <token>", except GET /healthz so container health checks work without the
// secret. An empty token disables the check.
func RequireBearer(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="codesynapse"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// NewServer returns an http.Server with conservative timeouts.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}
}

// Serve runs srv until ctx is cancelled, then shuts it down gracefully.
func Serve(ctx context.Context, srv *http.Server) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx) //nolint:contextcheck // parent ctx is already cancelled
	}
}
