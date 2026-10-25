// Package milvus implements vector.Index on Milvus 2.6 through its RESTful
// v2 API (standard library only; the Go SDK pulls in hundreds of packages
// and a cgo CPU probe).
//
// One collection per embedding model and vector length (the length is fixed
// per collection), named from a hash of the model so model strings never
// reach Milvus names. tenant_id is the partition key: each organisation's
// vectors sit in their own partitions and every search is routed there.
// Filters are expression templates; values only ever travel in exprParams,
// never inside the expression text.
package milvus

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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/vector"
)

const (
	defaultPrefix = "cs_memory_"
	// Field lengths: entity IDs are hashes, tenants organisation IDs,
	// namespaces "org|graph".
	maxID        = 128
	maxTenant    = 128
	maxNamespace = 1024
	// searchFilter routes a search to one tenant and namespace.
	searchFilter = "tenant_id == {tenant} && ns == {ns}"
	maxResponse  = 16 << 20
	upsertBatch  = 500
	// retryBudget bounds retries of transient failures: a clustered Milvus
	// answers "not ready" or drops connections while a node fails over.
	// Every call is safe to repeat: upserts and deletes go by ID, searches
	// read, and collection setup checks before it creates.
	retryBudget = 15 * time.Second
)

// retriable are Milvus error codes for a service, replica, channel or node
// that is not available yet (merr: ServiceNotReady, ServiceUnavailable,
// RequestLimitExceeded, ServiceRateLimit, ReplicaNotAvailable,
// ChannelNotAvailable, NodeNotFound, NodeOffline, NodeLack,
// NodeNotAvailable).
var retriable = []int{1, 2, 4, 8, 402, 503, 901, 902, 903, 905}

// Config holds connection settings. Token is never logged.
type Config struct {
	// URL is the REST endpoint, e.g. http://milvus:19530.
	URL string
	// Token is an API key or "user:password"; empty without auth.
	Token  string
	DBName string
	// Prefix separates installations sharing a Milvus (tests use their own).
	Prefix  string
	Timeout time.Duration
}

// Index is a Milvus-backed vector.Index.
type Index struct {
	base   string
	token  string
	db     string
	prefix string
	http   *http.Client

	mu          sync.Mutex
	ready       map[string]bool // collections known to exist and be loaded
	sessionMode bool
	sessions    *Index
	// retries counts retried calls (tests read it).
	retries atomic.Int64
}

var (
	_ vector.Index  = (*Index)(nil)
	_ vector.Prober = (*Index)(nil)
)

// New checks the URL and that Milvus answers.
func New(ctx context.Context, cfg Config) (*Index, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("milvus url must be http(s)://host:port without credentials")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	p := cfg.Prefix
	if p == "" {
		p = defaultPrefix
	}
	x := &Index{base: strings.TrimRight(cfg.URL, "/"), token: cfg.Token, db: cfg.DBName, prefix: p,
		http: &http.Client{Timeout: timeout}, ready: map[string]bool{}}
	x.sessions = &Index{base: x.base, token: x.token, db: x.db, prefix: "session_" + p, http: x.http, ready: map[string]bool{}, sessionMode: true}
	if _, err := x.names(ctx); err != nil {
		return nil, fmt.Errorf("connecting to milvus at %s: %w", cfg.URL, err)
	}
	return x, nil
}

// Close implements vector.Index.
func (x *Index) Close(context.Context) error { x.http.CloseIdleConnections(); return nil }

// call posts body to /v2/vectordb/path and decodes "data" into out. The
// path (a fixed set) is the operation label.
func (x *Index) call(ctx context.Context, path string, body map[string]any, out any) error {
	start := time.Now()
	err := x.do(ctx, path, body, out)
	for wait := 200 * time.Millisecond; transient(err) && time.Since(start)+wait < retryBudget; wait = min(2*wait, 2*time.Second) {
		select {
		case <-ctx.Done():
		case <-time.After(wait):
			x.retries.Add(1)
			err = x.do(ctx, path, body, out)
			continue
		}
		break
	}
	tenant := telemetry.Tenant(ctx)
	telemetry.DBDuration.ObserveContext(ctx, time.Since(start).Seconds(), tenant, "milvus", path)
	if err != nil {
		telemetry.DBErrors.IncContext(ctx, tenant, "milvus", path)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			telemetry.DBTimeouts.IncContext(ctx, tenant, "milvus", path)
		}
	}
	return err
}

func (x *Index) do(ctx context.Context, path string, body map[string]any, out any) error {
	if x.db != "" {
		body["dbName"] = x.db
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, x.base+"/v2/vectordb/"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if x.token != "" {
		req.Header.Set("Authorization", "Bearer "+x.token)
	}
	res, err := x.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close() //nolint:errcheck // read-only body
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return err
	}
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return &apiError{path: path, status: res.StatusCode, msg: "unreadable answer"}
	}
	if res.StatusCode != http.StatusOK || env.Code != 0 {
		return &apiError{path: path, status: res.StatusCode, code: env.Code, msg: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

type apiError struct {
	path   string
	status int
	code   int
	msg    string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("milvus %s: HTTP %d, code %d: %s", e.path, e.status, e.code, e.msg)
}

// transient reports a failure worth retrying: a connection that failed, a
// gateway or server error, or Milvus saying a part is not available yet.
// Cancellation, deadlines and answers about the request itself are final.
func transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.status >= 500 || slices.Contains(retriable, ae.code)
	}
	var ue *url.Error
	return errors.As(err, &ue)
}

// gone reports Milvus answering that a collection is missing or not loaded
// (dropped or released behind our back, or Milvus restarted).
func gone(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.code == 100 || ae.code == 101)
}

// forget drops name from the ready cache so the next ensure recreates or
// reloads it.
func (x *Index) forget(name string) {
	x.mu.Lock()
	delete(x.ready, name)
	x.mu.Unlock()
}

// collection names the collection of model's vectors of length dims.
func (x *Index) collection(model string, dims int) string {
	h := sha256.Sum256([]byte(model))
	return fmt.Sprintf("%s%s_%d", x.prefix, hex.EncodeToString(h[:8]), dims)
}

func (x *Index) names(ctx context.Context) ([]string, error) {
	var names []string
	err := x.call(ctx, "collections/list", map[string]any{}, &names)
	return names, err
}

func (x *Index) exists(ctx context.Context, name string) (bool, error) {
	var has struct {
		Has bool `json:"has"`
	}
	err := x.call(ctx, "collections/has", map[string]any{"collectionName": name}, &has)
	return has.Has, err
}

func (x *Index) ensure(ctx context.Context, name string, dims int) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.ready[name] {
		return nil
	}
	has, err := x.exists(ctx, name)
	if err != nil {
		return err
	}
	if !has {
		varchar := func(name string, n int) map[string]any {
			return map[string]any{"fieldName": name, "dataType": "VarChar", "elementTypeParams": map[string]any{"max_length": n}}
		}
		id, tenant := varchar("id", maxID), varchar("tenant_id", maxTenant)
		if x.sessionMode {
			id = varchar("id", 2048)
		}
		id["isPrimary"] = true
		tenant["isPartitionKey"] = true
		fields := []any{id, tenant, varchar("ns", maxNamespace), map[string]any{"fieldName": "vec", "dataType": "FloatVector", "elementTypeParams": map[string]any{"dim": fmt.Sprint(dims)}}}
		if x.sessionMode {
			fields = append(fields, varchar("session_id", 128), varchar("entity_id", 2048), map[string]any{"fieldName": "expires_at", "dataType": "Int64"})
		}
		err := x.call(ctx, "collections/create", map[string]any{
			"collectionName": name,
			"description":    "CodeSynapse agent-memory entity embeddings",
			"schema":         map[string]any{"autoId": false, "fields": fields},
			"indexParams": []any{map[string]any{"fieldName": "vec", "indexName": "vec", "metricType": "COSINE", "indexType": "HNSW",
				"params": map[string]any{"M": 16, "efConstruction": 200}}},
		}, nil)
		if err != nil {
			return fmt.Errorf("creating collection: %w", err)
		}
	}
	if err := x.call(ctx, "collections/load", map[string]any{"collectionName": name}, nil); err != nil {
		return fmt.Errorf("loading collection: %w", err)
	}
	x.ready[name] = true
	return nil
}

// Upsert implements vector.Index. An entity re-embedded by a model with
// another length lands in another collection; Delete clears the old one.
func (x *Index) Upsert(ctx context.Context, model string, items []vector.Item) error {
	if len(items) == 0 {
		return nil
	}
	if err := vector.Check(items); err != nil {
		return err
	}
	rows := make([]map[string]any, len(items))
	for i, it := range items {
		idLimit := maxID
		if x.sessionMode {
			idLimit = 2048
		} else if it.SessionID != "" {
			return sessionmemory.ErrScope
		}
		if len(it.ID) > idLimit || len(it.Tenant) > maxTenant || len(it.Namespace) > maxNamespace || len(it.SessionID) > 128 {
			return fmt.Errorf("milvus: item %q has an over-long field", it.ID)
		}
		rows[i] = map[string]any{"id": it.ID, "tenant_id": it.Tenant, "ns": it.Namespace, "vec": it.Vector}
		if x.sessionMode {
			rows[i]["session_id"] = it.SessionID
			rows[i]["entity_id"] = it.ID
			rows[i]["expires_at"] = it.ExpiresAt
			key, _ := json.Marshal([]string{it.Tenant, it.SessionID, it.Namespace, it.ID})
			sum := sha256.Sum256(key)
			rows[i]["id"] = hex.EncodeToString(sum[:])
		}
	}
	dims := len(items[0].Vector)
	name := x.collection(model, dims)
	if err := x.ensure(ctx, name, dims); err != nil {
		return err
	}
	for start := 0; start < len(rows); start += upsertBatch {
		body := map[string]any{"collectionName": name, "data": rows[start:min(start+upsertBatch, len(rows))]}
		err := x.call(ctx, "entities/upsert", body, nil)
		if gone(err) {
			x.forget(name)
			if err = x.ensure(ctx, name, dims); err == nil {
				err = x.call(ctx, "entities/upsert", body, nil)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Search implements vector.Index.
func (x *Index) Search(ctx context.Context, model, tenant, namespace string, vec []float32, k int) ([]vector.Hit, error) {
	if tenant == "" {
		return nil, vector.ErrNoTenant
	}
	if len(vec) < 1 || len(vec) > memory.MaxDimensions {
		return nil, nil
	}
	k = vector.ClampK(k)
	name := x.collection(model, len(vec))
	has, err := x.exists(ctx, name)
	if err != nil || !has {
		return nil, err // nothing embedded by this model at this length yet
	}
	if err := x.ensure(ctx, name, len(vec)); err != nil {
		return nil, err
	}
	var rows []struct {
		ID       string  `json:"id"`
		Tenant   string  `json:"tenant_id"`
		NS       string  `json:"ns"`
		Distance float64 `json:"distance"`
	}
	body := map[string]any{
		"collectionName": name, "data": [][]float32{vec}, "annsField": "vec", "limit": k,
		"filter": searchFilter, "exprParams": map[string]any{"tenant": tenant, "ns": namespace},
		"outputFields": []string{"tenant_id", "ns"}, "consistencyLevel": "Strong",
	}
	err = x.call(ctx, "entities/search", body, &rows)
	if gone(err) {
		x.forget(name)
		if has, herr := x.exists(ctx, name); herr != nil || !has {
			return nil, herr // dropped: nothing to find
		}
		if err = x.ensure(ctx, name, len(vec)); err == nil {
			err = x.call(ctx, "entities/search", body, &rows)
		}
	}
	if err != nil {
		return nil, err
	}
	hits := make([]vector.Hit, 0, len(rows))
	for _, r := range rows {
		// COSINE "distance" is cos in -1..1; memory.Similarity is (1+cos)/2.
		hits = append(hits, vector.Hit{ID: r.ID, Tenant: r.Tenant, Namespace: r.NS, Score: (1 + r.Distance) / 2})
	}
	hits = vector.Isolate(hits, tenant, namespace, k)
	telemetry.VectorMatches.AddContext(ctx, float64(len(hits)), telemetry.TenantLabel(orgOf(tenant)), "milvus")
	return hits, nil
}

// hasFilter routes a presence check to one tenant, namespace and ID list.
const hasFilter = "tenant_id == {tenant} && ns == {ns} && id in {ids}"

// Has implements vector.Prober. It asks every collection of the model (one
// per vector length) in batches, so the answer needs no length.
func (x *Index) Has(ctx context.Context, model, tenant, namespace string, ids []string) (map[string]bool, error) {
	if tenant == "" {
		return nil, vector.ErrNoTenant
	}
	out := map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	sum := sha256.Sum256([]byte(model))
	prefix := x.prefix + hex.EncodeToString(sum[:8]) + "_"
	names, err := x.names(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		for start := 0; start < len(ids); start += upsertBatch {
			batch := ids[start:min(start+upsertBatch, len(ids))]
			var rows []struct {
				ID string `json:"id"`
			}
			body := map[string]any{"collectionName": name, "filter": hasFilter, "limit": len(batch), "outputFields": []string{"id"},
				"exprParams": map[string]any{"tenant": tenant, "ns": namespace, "ids": batch}, "consistencyLevel": "Strong"}
			if err := x.call(ctx, "entities/query", body, &rows); err != nil {
				if gone(err) {
					x.forget(name)
					continue
				}
				return nil, err
			}
			for _, r := range rows {
				out[r.ID] = true
			}
		}
	}
	return out, nil
}

// orgOf turns a vector tenant back into an organisation ID ("" for none).
func orgOf(tenant string) string {
	if tenant == memory.NoTenant {
		return ""
	}
	return tenant
}

// Delete implements vector.Index.
func (x *Index) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	names, err := x.names(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if !strings.HasPrefix(name, x.prefix) {
			continue
		}
		if err := x.call(ctx, "entities/delete", map[string]any{"collectionName": name, "filter": "id in {ids}",
			"exprParams": map[string]any{"ids": ids}}, nil); err != nil {
			return fmt.Errorf("deleting from %s: %w", name, err)
		}
	}
	return nil
}

// Drop removes every collection of this index's prefix (tests only).
func (x *Index) Drop(ctx context.Context) error {
	names, err := x.names(ctx)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, name := range names {
		if strings.HasPrefix(name, x.prefix) || strings.HasPrefix(name, "session_"+x.prefix) {
			if err := x.call(ctx, "collections/drop", map[string]any{"collectionName": name}, nil); err != nil {
				return err
			}
			delete(x.ready, name)
		}
	}
	return nil
}
