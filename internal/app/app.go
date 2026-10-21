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
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/neo4jstore"
)

// Environment variables. The password is only read from the environment so
// it never appears in process listings or shell history.
const (
	EnvNeo4jURI      = "CODESYNAPSE_NEO4J_URI"
	EnvNeo4jUser     = "CODESYNAPSE_NEO4J_USER"
	EnvNeo4jPassword = "CODESYNAPSE_NEO4J_PASSWORD" //nolint:gosec // env var name, not a credential
	EnvNeo4jDatabase = "CODESYNAPSE_NEO4J_DATABASE"
)

// Neo4jFlags are the connection settings shared by all binaries.
type Neo4jFlags struct {
	URI      string
	User     string
	Database string
}

// AddFlags registers the flags on fs with environment-derived defaults.
func (f *Neo4jFlags) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&f.URI, "neo4j-uri", Getenv(EnvNeo4jURI, "neo4j://localhost:7687"), "Neo4j URI (env "+EnvNeo4jURI+")")
	fs.StringVar(&f.User, "neo4j-user", Getenv(EnvNeo4jUser, "neo4j"), "Neo4j user (env "+EnvNeo4jUser+")")
	fs.StringVar(&f.Database, "neo4j-database", Getenv(EnvNeo4jDatabase, "neo4j"), "Neo4j database (env "+EnvNeo4jDatabase+")")
}

// Open connects to Neo4j.
func (f *Neo4jFlags) Open(ctx context.Context) (store.Store, error) {
	pw := os.Getenv(EnvNeo4jPassword)
	if pw == "" {
		return nil, fmt.Errorf("%s is not set", EnvNeo4jPassword)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return neo4jstore.New(ctx, neo4jstore.Config{URI: f.URI, User: f.User, Password: pw, Database: f.Database})
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
