// Package app holds wiring shared by the codesynapse binaries: graph store
// flags (HugeGraph and Milvus), store construction and HTTP hardening helpers.
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
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/internal/vector/milvus"
)

// Environment variables. The password is only read from the environment so
// it never appears in process listings or shell history.
const (
	// EnvGraphBackend once chose between Neo4j and HugeGraph. HugeGraph is
	// now the only graph database; any other value is refused so an
	// upgraded installation does not silently start on an empty graph.
	EnvGraphBackend      = "CODESYNAPSE_GRAPH_BACKEND"
	EnvHugeGraphURL      = "CODESYNAPSE_HUGEGRAPH_URL"
	EnvHugeGraphGraph    = "CODESYNAPSE_HUGEGRAPH_GRAPH"
	EnvHugeGraphUser     = "CODESYNAPSE_HUGEGRAPH_USER"
	EnvHugeGraphPassword = "CODESYNAPSE_HUGEGRAPH_PASSWORD" //nolint:gosec // env var name, not a credential
	// EnvHugeGraphMaxRequest is the server's Gremlin maxContentLength in
	// bytes (default 60000, under HugeGraph's 65536).
	EnvHugeGraphMaxRequest = "CODESYNAPSE_HUGEGRAPH_MAX_REQUEST_BYTES"
	EnvHugeGraphTxCapacity = "CODESYNAPSE_HUGEGRAPH_TX_CAPACITY"
	// EnvMilvusURL turns on Milvus for agent-memory vectors.
	EnvMilvusURL   = "CODESYNAPSE_MILVUS_URL"
	EnvMilvusToken = "CODESYNAPSE_MILVUS_TOKEN" //nolint:gosec // env var name, not a credential
)

// GraphFlags are the graph store settings shared by all binaries: HugeGraph,
// with Milvus for agent-memory vectors.
type GraphFlags struct {
	Backend        string
	HugeGraphURL   string
	HugeGraphGraph string
	MilvusURL      string
}

// AddFlags registers the flags on fs with environment-derived defaults.
func (f *GraphFlags) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&f.Backend, "graph-backend", Getenv(EnvGraphBackend, "hugegraph"), "Graph database; only hugegraph is supported (env "+EnvGraphBackend+")")
	fs.StringVar(&f.HugeGraphURL, "hugegraph-url", Getenv(EnvHugeGraphURL, "http://localhost:8180"), "HugeGraph server URL (env "+EnvHugeGraphURL+"; user and password from "+EnvHugeGraphUser+"/"+EnvHugeGraphPassword+")")
	fs.StringVar(&f.HugeGraphGraph, "hugegraph-graph", Getenv(EnvHugeGraphGraph, "hugegraph"), "HugeGraph graph name (env "+EnvHugeGraphGraph+")")
	fs.StringVar(&f.MilvusURL, "milvus-url", Getenv(EnvMilvusURL, ""), "Milvus REST URL for agent-memory vectors, e.g. http://milvus:19530 (env "+EnvMilvusURL+"; token from "+EnvMilvusToken+")")
}

// ErrNeo4jRemoved refuses a configuration that still asks for Neo4j.
var ErrNeo4jRemoved = errors.New("the Neo4j graph backend was removed: run HugeGraph (README, \"Graph database\"), unset " + EnvGraphBackend + " and re-index")

// Open connects to HugeGraph.
func (f *GraphFlags) Open(ctx context.Context) (store.Store, error) {
	switch f.Backend {
	case "", "hugegraph":
		return f.openHugeGraph(ctx)
	case "neo4j":
		return nil, ErrNeo4jRemoved
	default:
		return nil, fmt.Errorf("unknown graph backend %q: only hugegraph is supported (%s)", f.Backend, EnvGraphBackend)
	}
}

func (f *GraphFlags) openHugeGraph(ctx context.Context) (store.Store, error) {
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
// so they can start before HugeGraph finishes booting under docker compose.
func (f *GraphFlags) OpenWithRetry(ctx context.Context, onRetry func(error)) (store.Store, error) {
	if f.Backend != "" && f.Backend != "hugegraph" {
		return f.Open(ctx) // a configuration error: retrying cannot help
	}
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
