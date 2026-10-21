// Package store defines the persistence contract for code graphs. The Neo4j
// implementation is the production backend; the in-memory implementation
// backs tests and offline use. Both must pass storetest.Run.
package store

import (
	"context"
	"errors"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
)

// ErrNotFound is returned when a requested node or repository does not exist.
var ErrNotFound = errors.New("not found")

// Limits shared by all implementations so a single query cannot return an
// unbounded result to an API client or an agent.
const (
	MaxDepth = 6
	MaxLimit = 500
	// MaxSubgraphEdges bounds Subgraph results (graph views).
	MaxSubgraphEdges = 5000
)

// Direction selects which way a traversal follows edges.
type Direction string

// Directions.
const (
	Out Direction = "out"
	In  Direction = "in"
)

// Metric orders List results.
type Metric string

// Metrics.
const (
	ByName       Metric = "name"
	ByComplexity Metric = "complexity"
	ByChurn      Metric = "churn"
	ByLOC        Metric = "loc"
	// ByFanIn counts incoming CALLS edges.
	ByFanIn Metric = "fan_in"
	// ByTime orders by Time, newest first (commits).
	ByTime Metric = "time"
	// ByAuthored counts outgoing AUTHORED edges (authors).
	ByAuthored Metric = "authored"
)

// Valid reports whether m is a known metric.
func (m Metric) Valid() bool {
	switch m {
	case ByName, ByComplexity, ByChurn, ByLOC, ByFanIn, ByTime, ByAuthored:
		return true
	}
	return false
}

// Filter narrows List results. Zero fields do not filter.
type Filter struct {
	Kinds []model.NodeKind
	// Tag keeps nodes carrying this security tag.
	Tag string
	// FilePrefix keeps nodes whose File starts with this prefix.
	FilePrefix string
	// ExportedOnly keeps exported symbols.
	ExportedOnly bool
	// Undocumented keeps nodes without a Summary.
	Undocumented bool
	// ExcludeTests drops test functions and test files.
	ExcludeTests bool
}

// Ranked is a node with the value it was ranked by.
type Ranked struct {
	Node  model.Node `json:"node"`
	Score int        `json:"score"`
}

// Writer persists graphs.
type Writer interface {
	// EnsureSchema creates constraints and indexes. It is idempotent.
	EnsureSchema(ctx context.Context) error
	// ReplaceRepo atomically swaps the stored graph for repo with g. Nodes
	// and edges no longer present in g are removed.
	ReplaceRepo(ctx context.Context, repo string, g *model.Graph) error
	// DeleteRepo removes every node of repo.
	DeleteRepo(ctx context.Context, repo string) error
}

// Reader answers graph queries. Every method is bounded by MaxLimit and
// MaxDepth; implementations clamp out-of-range values instead of failing.
type Reader interface {
	// Repos lists Repository nodes.
	Repos(ctx context.Context) ([]model.Node, error)
	// Stats summarises one repository.
	Stats(ctx context.Context, repo string) (model.Stats, error)
	// Node returns a node by ID, or ErrNotFound.
	Node(ctx context.Context, id string) (model.Node, error)
	// Search finds nodes whose name or qualified name contains q
	// (case-insensitive). Exact name matches rank first.
	Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error)
	// NodesInFile returns the code symbols and doc sections defined in file,
	// ordered by start line.
	NodesInFile(ctx context.Context, repo, file string) ([]model.Node, error)
	// Traverse follows edges of the given kinds from id up to depth hops and
	// returns each reachable node once, at its shortest distance.
	Traverse(ctx context.Context, id string, edges []model.EdgeKind, dir Direction, depth, limit int) ([]model.Hit, error)
	// List returns nodes of repo matching f, ordered by m (descending for
	// numeric metrics).
	List(ctx context.Context, repo string, f Filter, m Metric, limit int) ([]Ranked, error)
	// Subgraph returns the edges whose endpoints are both in ids, optionally
	// restricted to kinds. At most MaxSubgraphEdges are returned.
	Subgraph(ctx context.Context, ids []string, kinds []model.EdgeKind) ([]model.Edge, error)
}

// ReviewStore persists code reviews (package reviews). Reviews are kept
// apart from repository graphs: ReplaceRepo never touches them, DeleteRepo
// does not either (a removed repository's reviews remain readable by ID).
type ReviewStore interface {
	SaveReview(ctx context.Context, r reviews.Review) error
	GetReview(ctx context.Context, id string) (reviews.Review, error)
	// ListReviews returns reviews of repo, newest first.
	ListReviews(ctx context.Context, repo string, limit int) ([]reviews.Review, error)
	DeleteReview(ctx context.Context, id string) error
}

// Store is a full read/write backend.
type Store interface {
	Reader
	Writer
	ReviewStore
	Close(ctx context.Context) error
}

// ClampLimit bounds limit to [1, MaxLimit], defaulting to def when <= 0.
func ClampLimit(limit, def int) int {
	if limit <= 0 {
		limit = def
	}
	return min(limit, MaxLimit)
}

// ClampDepth bounds depth to [1, MaxDepth].
func ClampDepth(depth int) int {
	return min(max(depth, 1), MaxDepth)
}
