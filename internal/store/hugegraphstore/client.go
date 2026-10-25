// Package hugegraphstore implements store.Store on Apache HugeGraph 1.5
// through its Gremlin endpoint (standard library only).
//
// Query safety, the Gremlin version of the Cypher rule: every script is a
// constant in this package and every value travels in "bindings", never in
// the script text. Labels are model allowlist values passed as bindings too.
//
// HugeGraph specifics the code relies on:
//   - Vertex IDs are hashes of the model IDs (custom string IDs are capped
//     at 128 bytes); the real ID is the "nid" property.
//   - Re-adding a vertex or edge with the same ID overwrites it, which is
//     how upserts work.
//   - Filtering on a property needs an index on it, unless the filter
//     follows an indexed lookup (not(), filter{}).
//   - Each request is one transaction, and index lookups are refused after
//     writes in the same request, so scripts read before they write.
//   - A map returned at the top level is unrolled into its entries, so
//     scripts return [[k: v]] to get one map back.
//   - Tenant-owned data (agent memory) carries tenant_id, and every
//     traversal over it starts at has('tenant_id', …).
package hugegraphstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/vector"
)

const maxResponse = 64 << 20

// Config holds connection settings. Password is never logged.
type Config struct {
	// URL is the server, e.g. http://hugegraph:8080.
	URL      string
	Graph    string // default "hugegraph"
	User     string // empty without authentication
	Password string
	// QueryTimeout bounds each read; writes get four times as long. 0 means 30s.
	QueryTimeout time.Duration
	// Vectors holds memory embeddings. Without one, recall by meaning is off.
	Vectors vector.Index
	// MaxRequestBytes bounds one request (script and bindings). HugeGraph's
	// gremlin-server.yaml maxContentLength defaults to 65536; writes are
	// batched to stay under this. 0 means 60000.
	MaxRequestBytes int
	// TxCapacity must match the server's vertex.tx_capacity and
	// edge.tx_capacity: a repository replacement is one transaction and is
	// refused, before writing, when it needs more operations. 0 means
	// HugeGraph's default, 10000.
	TxCapacity int
}

// defaultTxCapacity is HugeGraph's own vertex.tx_capacity and edge.tx_capacity;
// maxTxCapacity is the largest value HugeGraph 1.5 accepts for either.
const (
	defaultTxCapacity = 10000
	maxTxCapacity     = 1000000
)

// Store is a HugeGraph-backed store.Store.
type Store struct {
	sessionmemory.Engine
	base, graph string
	// binding is the graph's Gremlin binding: the graph name up to HugeGraph
	// 1.5, "<graph space>-<graph>" from 1.7 (see bindGraph).
	binding        string
	user, password string
	http           *http.Client
	timeout        time.Duration
	vec            vector.Index
	maxRequest     int
	txCapacity     int
	// lastPublish is "delta" or "full": how the last replacement was
	// written (tests and logs).
	lastPublish atomic.Value
	// published caches the hashes of the last commit per repository, so a
	// republish skips reading them back (delta.go).
	published publishedHashes
	hashReads atomic.Int64 // reads of every node's hashes (tests)
}

var _ store.Store = (*Store)(nil)

var graphName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,47}$`)

// New checks the settings and that HugeGraph answers.
func New(ctx context.Context, cfg Config) (*Store, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("hugegraph url must be http(s)://host:port without credentials")
	}
	if cfg.Graph == "" {
		cfg.Graph = "hugegraph"
	}
	if !graphName.MatchString(cfg.Graph) {
		return nil, fmt.Errorf("invalid hugegraph graph name %q", cfg.Graph)
	}
	timeout := cfg.QueryTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	s := &Store{base: strings.TrimRight(cfg.URL, "/"), graph: cfg.Graph, user: cfg.User, password: cfg.Password,
		http: &http.Client{}, timeout: timeout, vec: cfg.Vectors, maxRequest: cfg.MaxRequestBytes}
	s.Engine = sessionmemory.Engine{Backend: s}
	if s.maxRequest <= 0 {
		s.maxRequest = 60000
	}
	s.txCapacity = cfg.TxCapacity
	if s.txCapacity <= 0 {
		s.txCapacity = defaultTxCapacity
	}
	if s.txCapacity > maxTxCapacity {
		return nil, fmt.Errorf("hugegraph transaction capacity %d exceeds the server maximum %d", s.txCapacity, maxTxCapacity)
	}
	if err := s.bindGraph(ctx); err != nil {
		return nil, fmt.Errorf("connecting to hugegraph at %s: %w", cfg.URL, err)
	}
	return s, nil
}

// defaultGraphSpace is the graph space a standalone HugeGraph 1.7+ server
// puts its graphs in.
const defaultGraphSpace = "DEFAULT"

// bindGraph picks the name the server binds the graph under from its
// version, then checks that the graph answers. HugeGraph 1.7 binds
// "<graph space>-<graph>" (the plain name fails); earlier versions bind the
// plain name. While 1.7 is still loading both names can answer for a moment,
// so the version decides, not a probe; a graph that is not ready yet fails
// here and the services retry.
func (s *Store) bindGraph(ctx context.Context) error {
	core, err := s.coreVersion(ctx)
	if err != nil {
		return err
	}
	s.binding = graphBinding(core, s.graph)
	var one []int
	return s.read(ctx, "1", nil, &one)
}

// graphBinding returns the Gremlin binding of graph on a server whose core
// version is core ("1.7.0"); an unreadable version keeps the plain name.
func graphBinding(core, graph string) string {
	var major, minor int
	if _, err := fmt.Sscanf(core, "%d.%d", &major, &minor); err == nil && (major > 1 || major == 1 && minor >= 7) {
		return defaultGraphSpace + "-" + graph
	}
	return graph
}

// coreVersion asks the server for its core version.
func (s *Store) coreVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/versions", nil)
	if err != nil {
		return "", err
	}
	if s.user != "" {
		req.SetBasicAuth(s.user, s.password)
	}
	res, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close() //nolint:errcheck // read-only body
	var v struct {
		Versions struct {
			Core string `json:"core"`
		} `json:"versions"`
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hugegraph: versions: HTTP %d", res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&v); err != nil {
		return "", fmt.Errorf("hugegraph: versions: %w", err)
	}
	return v.Versions.Core, nil
}

// CodeVectors is the index of symbol embeddings (internal/semantic); nil
// without Milvus.
func (s *Store) CodeVectors() vector.Index { return s.vec }

// System names the database for metrics.
func (s *Store) System() string { return "hugegraph" }

// Close implements store.Store.
func (s *Store) Close(ctx context.Context) error {
	s.http.CloseIdleConnections()
	if s.vec != nil {
		return s.vec.Close(ctx)
	}
	return nil
}

func (s *Store) read(ctx context.Context, script string, bindings map[string]any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.timed(ctx, "read", script, bindings, out)
}

func (s *Store) write(ctx context.Context, script string, bindings map[string]any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 4*s.timeout)
	defer cancel()
	return s.timed(ctx, "write", script, bindings, out)
}

// timed records the round trip and, for reads, how many rows came back.
func (s *Store) timed(ctx context.Context, op, script string, bindings map[string]any, out any) error {
	// The span names the operation, never the script or its bindings.
	ctx, span := telemetry.Tracer().Start(ctx, "hugegraph "+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("db.system.name", "hugegraph"), attribute.String("db.operation.name", op)))
	defer span.End()
	start := time.Now()
	err := s.gremlin(ctx, script, bindings, out)
	if err != nil {
		span.SetStatus(codes.Error, op+" failed")
	}
	tenant := telemetry.Tenant(ctx)
	telemetry.DBDuration.ObserveContext(ctx, time.Since(start).Seconds(), tenant, "hugegraph", op)
	if err != nil {
		telemetry.DBErrors.IncContext(ctx, tenant, "hugegraph", op)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			telemetry.DBTimeouts.IncContext(ctx, tenant, "hugegraph", op)
		}
		return err
	}
	if v := reflect.ValueOf(out); op == "read" && v.Kind() == reflect.Pointer && v.Elem().Kind() == reflect.Slice {
		telemetry.GraphTraversed.AddContext(ctx, float64(v.Elem().Len()), tenant, "hugegraph")
		telemetry.GraphRowsReturned.AddContext(ctx, float64(v.Elem().Len()), tenant, "hugegraph")
	}
	return nil
}

// gremlin runs a constant script with bindings and decodes result.data.
func (s *Store) gremlin(ctx context.Context, script string, bindings map[string]any, out any) error {
	if bindings == nil {
		bindings = map[string]any{}
	}
	body, err := s.gremlinBody(script, bindings)
	if err != nil {
		return err
	}
	if len(body) > s.maxRequest {
		return fmt.Errorf("hugegraph: request of %d bytes exceeds the %d-byte limit (raise maxContentLength in gremlin-server.yaml and %s)",
			len(body), s.maxRequest, "CODESYNAPSE_HUGEGRAPH_MAX_REQUEST_BYTES")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/gremlin", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.user != "" {
		req.SetBasicAuth(s.user, s.password)
	}
	res, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // read-only body
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return err
	}
	var env struct {
		Exception string `json:"exception"`
		Message   string `json:"message"`
		Result    struct {
			Data json.RawMessage `json:"data"`
		} `json:"result"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil {
		return fmt.Errorf("hugegraph: HTTP %d, unreadable answer", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK || env.Exception != "" {
		return fmt.Errorf("hugegraph: HTTP %d: %s", res.StatusCode, firstLine(env.Message))
	}
	if out == nil {
		return nil
	}
	dec = json.NewDecoder(bytes.NewReader(env.Result.Data))
	dec.UseNumber()
	return dec.Decode(out)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s[:min(len(s), 300)]
}

// vid is the HugeGraph vertex ID of a model ID; prefix keeps labels apart.
func vid(prefix, id string) string {
	h := sha256.Sum256([]byte(id))
	return prefix + hex.EncodeToString(h[:16])
}

func nodeVID(id string) string { return vid("n", id) }

func lower(s string) string { return strings.ToLower(s) }

func (s *Store) gremlinBody(script string, bindings map[string]any) ([]byte, error) {
	return json.Marshal(map[string]any{
		"gremlin": script, "bindings": bindings, "language": "gremlin-groovy",
		"aliases": map[string]string{"graph": s.binding, "g": "__g_" + s.binding},
	})
}
