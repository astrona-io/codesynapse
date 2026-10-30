// Package app holds wiring shared by the codesynapse binaries: graph store
// flags (NebulaGraph and Milvus), store construction and HTTP hardening helpers.
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
	"github.com/astrona-io/codesynapse/internal/store/nebulastore"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/internal/vector/milvus"
)

// Environment variables. The password is only read from the environment so
// it never appears in process listings or shell history.
const (
	// EnvGraphBackend once chose between Neo4j, HugeGraph and NebulaGraph.
	// NebulaGraph is now the only graph database; any other value is refused
	// so an upgraded installation does not silently start on an empty graph.
	EnvGraphBackend    = "CODESYNAPSE_GRAPH_BACKEND"
	EnvNebulaAddrs     = "CODESYNAPSE_NEBULA_ADDRS"
	EnvNebulaSpace     = "CODESYNAPSE_NEBULA_SPACE"
	EnvNebulaUser      = "CODESYNAPSE_NEBULA_USER"
	EnvNebulaPassword  = "CODESYNAPSE_NEBULA_PASSWORD" //nolint:gosec // env var name, not a credential
	EnvNebulaPartition = "CODESYNAPSE_NEBULA_PARTITIONS"
	EnvNebulaReplicas  = "CODESYNAPSE_NEBULA_REPLICAS"
	// EnvMilvusURL turns on Milvus for agent-memory vectors.
	EnvMilvusURL   = "CODESYNAPSE_MILVUS_URL"
	EnvMilvusToken = "CODESYNAPSE_MILVUS_TOKEN" //nolint:gosec // env var name, not a credential
)

// GraphFlags are the graph store settings shared by all binaries: NebulaGraph,
// with Milvus for agent-memory vectors.
type GraphFlags struct {
	Backend     string
	NebulaAddrs string
	NebulaSpace string
	MilvusURL   string
}

// AddFlags registers the flags on fs with environment-derived defaults.
func (f *GraphFlags) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&f.Backend, "graph-backend", Getenv(EnvGraphBackend, "nebula"), "Graph database; only nebula is supported (env "+EnvGraphBackend+")")
	fs.StringVar(&f.NebulaAddrs, "nebula-addrs", Getenv(EnvNebulaAddrs, "localhost:9669"), "NebulaGraph graphd addresses, comma separated (env "+EnvNebulaAddrs+"; user and password from "+EnvNebulaUser+"/"+EnvNebulaPassword+")")
	fs.StringVar(&f.NebulaSpace, "nebula-space", Getenv(EnvNebulaSpace, "codesynapse"), "NebulaGraph space name (env "+EnvNebulaSpace+")")
	fs.StringVar(&f.MilvusURL, "milvus-url", Getenv(EnvMilvusURL, ""), "Milvus REST URL for agent-memory vectors, e.g. http://milvus:19530 (env "+EnvMilvusURL+"; token from "+EnvMilvusToken+")")
}

// ErrBackendRemoved refuses a configuration that still asks for a graph
// database CodeSynapse no longer supports.
var ErrBackendRemoved = errors.New("this graph backend was removed: run NebulaGraph (README, \"Graph database\"), unset " + EnvGraphBackend + " and re-index")

// Open connects to NebulaGraph.
func (f *GraphFlags) Open(ctx context.Context) (store.Store, error) {
	switch f.Backend {
	case "", "nebula":
		return f.openNebula(ctx)
	case "neo4j", "hugegraph":
		return nil, ErrBackendRemoved
	default:
		return nil, fmt.Errorf("unknown graph backend %q: only nebula is supported (%s)", f.Backend, EnvGraphBackend)
	}
}

func (f *GraphFlags) openNebula(ctx context.Context) (store.Store, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	password, err := nebulaPassword()
	if err != nil {
		return nil, err
	}
	partitions, err := strconv.Atoi(Getenv(EnvNebulaPartition, "0"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvNebulaPartition, err)
	}
	replicas, err := strconv.Atoi(Getenv(EnvNebulaReplicas, "0"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvNebulaReplicas, err)
	}
	var vec vector.Index
	if f.MilvusURL != "" {
		mx, err := milvus.New(ctx, milvus.Config{URL: f.MilvusURL, Token: os.Getenv(EnvMilvusToken)})
		if err != nil {
			return nil, err
		}
		vec = mx
	}
	s, err := nebulastore.New(ctx, nebulastore.Config{Addrs: strings.Split(f.NebulaAddrs, ","), Space: f.NebulaSpace,
		User: Getenv(EnvNebulaUser, "root"), Password: password, Vectors: vec, Partitions: partitions, Replicas: replicas})
	if err != nil {
		if vec != nil {
			_ = vec.Close(ctx) //nolint:errcheck // the connection error is the one worth returning
		}
		return nil, err
	}
	return s, nil
}

// MaxNebulaPassword is the longest password NebulaGraph accepts.
const MaxNebulaPassword = 24

// ErrNebulaPassword refuses a missing or overlong password: authentication
// is always on, and retrying cannot fix either.
var ErrNebulaPassword = errors.New("invalid NebulaGraph password")

func nebulaPassword() (string, error) {
	p := os.Getenv(EnvNebulaPassword)
	switch {
	case p == "":
		return "", fmt.Errorf("%w: %s is not set", ErrNebulaPassword, EnvNebulaPassword)
	case len(p) > MaxNebulaPassword:
		return "", fmt.Errorf("%w: %s is longer than %d characters, NebulaGraph's limit", ErrNebulaPassword, EnvNebulaPassword, MaxNebulaPassword)
	}
	return p, nil
}

// OpenWithRetry retries Open until it succeeds or ctx ends. Services use it
// so they can start before NebulaGraph finishes booting under docker compose.
func (f *GraphFlags) OpenWithRetry(ctx context.Context, onRetry func(error)) (store.Store, error) {
	if f.Backend != "" && f.Backend != "nebula" {
		return f.Open(ctx) // a configuration error: retrying cannot help
	}
	delay := time.Second
	for {
		s, err := f.Open(ctx)
		if err == nil {
			return s, nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrNebulaPassword) || strings.Contains(err.Error(), "is not set") {
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

// SchemaWait is how long a service that does not own the schema waits for
// the one that does (the API) to create or upgrade it.
const SchemaWait = 10 * time.Minute

// WaitForSchema checks the schema until it is complete, without changing it.
// Workers and the MCP service call it instead of EnsureSchema: their database
// user may not change the schema, and the API service, which owns it, may
// still be creating it. It gives up after wait with an error that says who
// creates the schema.
func WaitForSchema(ctx context.Context, st store.Writer, wait time.Duration, onRetry func(error)) error {
	deadline := time.Now().Add(wait)
	delay := time.Second
	for {
		err := st.CheckSchema(ctx)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the graph schema is still not ready after %s; the API service creates and upgrades it at start, so check that it runs and its log: %w", wait, err)
		}
		onRetry(err)
		select {
		case <-ctx.Done():
			return ctx.Err()
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
