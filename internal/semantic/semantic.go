// Package semantic ranks code symbols by meaning: the name, signature and
// first documentation sentence of each symbol are embedded by a local model
// (never source bodies, never a cloud provider) and compared with the task.
// The index is exact and in-process; it is an experiment for roadmap item 6
// (docs/PLAN.md) until the task eval shows it pays for itself.
package semantic

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Embedder turns texts into vectors, one per text, in order.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Hit is a ranked symbol.
type Hit struct {
	ID    string
	Score float64 // cosine similarity
}

// Index holds the normalised vectors of a repository's symbols.
type Index struct {
	emb  Embedder
	ids  []string
	vecs [][]float32
}

const (
	batch   = 64
	maxText = 400
)

// Text is what is embedded for a symbol: kind, name, signature and summary,
// all through redact.Summary.
func Text(n model.Node) string {
	parts := []string{string(n.Kind), n.QualifiedName}
	if n.Signature != "" {
		parts = append(parts, n.Signature)
	}
	if n.Summary != "" {
		parts = append(parts, n.Summary)
	}
	return "search_document: " + redact.Summary(strings.Join(parts, " "), maxText)
}

// Build embeds nodes into an index.
func Build(ctx context.Context, emb Embedder, nodes []model.Node) (*Index, error) {
	ix := &Index{emb: emb}
	for off := 0; off < len(nodes); off += batch {
		part := nodes[off:min(off+batch, len(nodes))]
		texts := make([]string, len(part))
		for i, n := range part {
			texts[i] = Text(n)
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return nil, err
		}
		if len(vecs) != len(part) {
			return nil, fmt.Errorf("semantic: %d vectors for %d texts", len(vecs), len(part))
		}
		for i, v := range vecs {
			ix.ids = append(ix.ids, part[i].ID)
			ix.vecs = append(ix.vecs, normalise(v))
		}
	}
	return ix, nil
}

// Rank returns the k symbols nearest to the task, best first.
func (ix *Index) Rank(ctx context.Context, task string, k int) ([]Hit, error) {
	vecs, err := ix.emb.Embed(ctx, []string{"search_query: " + redact.Summary(task, 2000)})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("semantic: no query vector")
	}
	q := normalise(vecs[0])
	hits := make([]Hit, 0, len(ix.ids))
	for i, v := range ix.vecs {
		if len(v) != len(q) {
			continue
		}
		var dot float64
		for j := range v {
			dot += float64(v[j]) * float64(q[j])
		}
		hits = append(hits, Hit{ID: ix.ids[i], Score: dot})
	}
	slices.SortFunc(hits, func(a, b Hit) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
	return hits[:min(k, len(hits))], nil
}

// Len is how many symbols are indexed.
func (ix *Index) Len() int { return len(ix.ids) }

func normalise(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	out := make([]float32, len(v))
	if sum == 0 {
		return out
	}
	inv := float32(1 / math.Sqrt(sum))
	for i, x := range v {
		out[i] = x * inv
	}
	return out
}

// Ollama embeds with a local Ollama server (/api/embed). Only loopback or
// private addresses are accepted: symbol names must not leave the machine.
type Ollama struct {
	URL, Model string
	HTTP       *http.Client
}

// NewOllama checks that baseURL is local.
func NewOllama(baseURL, model string) (*Ollama, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("semantic: bad Ollama URL %q", baseURL)
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || (!ip.IsLoopback() && !ip.IsPrivate())) {
		return nil, fmt.Errorf("semantic: %s is not a local address; embeddings run locally only", host)
	}
	return &Ollama{URL: strings.TrimRight(baseURL, "/"), Model: model, HTTP: &http.Client{Timeout: 2 * time.Minute}}, nil
}

// Embed implements Embedder.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": o.Model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := o.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() //nolint:errcheck // bounded read
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantic: Ollama answered %d", res.StatusCode)
	}
	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}
