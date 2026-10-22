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
	"strings"
	"sync"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/vector"
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
)

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

	mu    sync.Mutex
	ready map[string]bool // collections known to exist and be loaded
}

var _ vector.Index = (*Index)(nil)

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
	if _, err := x.names(ctx); err != nil {
		return nil, fmt.Errorf("connecting to milvus at %s: %w", cfg.URL, err)
	}
	return x, nil
}

// Close implements vector.Index.
func (x *Index) Close(context.Context) error { x.http.CloseIdleConnections(); return nil }

// call posts body to /v2/vectordb/path and decodes "data" into out.
func (x *Index) call(ctx context.Context, path string, body map[string]any, out any) error {
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
		return fmt.Errorf("milvus %s: HTTP %d, unreadable answer", path, res.StatusCode)
	}
	if res.StatusCode != http.StatusOK || env.Code != 0 {
		return &apiError{path: path, code: env.Code, msg: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

type apiError struct {
	path string
	code int
	msg  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("milvus %s: code %d: %s", e.path, e.code, e.msg)
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
		id["isPrimary"] = true
		tenant["isPartitionKey"] = true
		err := x.call(ctx, "collections/create", map[string]any{
			"collectionName": name,
			"description":    "CodeSynapse agent-memory entity embeddings",
			"schema": map[string]any{"autoId": false, "fields": []any{id, tenant, varchar("ns", maxNamespace),
				map[string]any{"fieldName": "vec", "dataType": "FloatVector", "elementTypeParams": map[string]any{"dim": fmt.Sprint(dims)}}}},
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
		if len(it.ID) > maxID || len(it.Tenant) > maxTenant || len(it.Namespace) > maxNamespace {
			return fmt.Errorf("milvus: item %q has an over-long field", it.ID)
		}
		rows[i] = map[string]any{"id": it.ID, "tenant_id": it.Tenant, "ns": it.Namespace, "vec": it.Vector}
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
	return vector.Isolate(hits, tenant, namespace, k), nil
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
		if strings.HasPrefix(name, x.prefix) {
			if err := x.call(ctx, "collections/drop", map[string]any{"collectionName": name}, nil); err != nil {
				return err
			}
			delete(x.ready, name)
		}
	}
	return nil
}
