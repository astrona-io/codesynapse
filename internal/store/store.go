// Package store defines the persistence contract for code graphs. The Neo4j
// implementation is the production backend; the in-memory implementation
// backs tests and offline use. Both must pass storetest.Run.
package store

import (
	"context"
	"errors"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/internal/usage"
)

// ErrNotFound is returned when a requested node or repository does not exist.
var ErrNotFound = errors.New("not found")

// ErrMemoryFull is returned when an upsert would take a repository's agent
// memory past memory.MaxFactsPerRepo or memory.MaxEntitiesPerRepo.
var ErrMemoryFull = memory.ErrFull

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
	// FindingCounts counts every finding of repo by severity (List stops
	// at MaxLimit; a grade must not).
	FindingCounts(ctx context.Context, repo string) (map[string]int, error)
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

// MemoryStore persists agent memory (package memory): entities and facts per
// repository, apart from repository graphs, which ReplaceRepo and DeleteRepo
// never touch.
//
// Search and subgraph results are ordered deterministically so the memory and
// Neo4j stores answer identically: entities by match score (query words found
// in the name, or equal to the type) then most recently updated then name;
// facts by most recently updated then ID.
type MemoryStore interface {
	// UpsertMemory resolves b's names against stored aliases, then applies
	// b.Plan: entities and facts merged, replaced facts closed (kept as
	// history), alias entities merged into their canonical one. All or
	// nothing; ErrMemoryFull over the limits.
	UpsertMemory(ctx context.Context, b memory.Batch) error
	// SearchMemory returns up to limit entities of repo whose name or an
	// alias contains a word of query (all entities, most recent first, when
	// query is empty) and the facts touching them, with those facts' other
	// entities. Only current facts unless history is set.
	SearchMemory(ctx context.Context, repo, query string, limit int, history bool) (memory.Graph, error)
	// MemorySubgraph returns facts with an endpoint within depth-1 hops of
	// the entity IDs (depth 1 = their own facts, at most 2), up to limit.
	// Hops and results use current facts only unless history is set.
	MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int, history bool) (memory.Graph, error)
	// DeleteMemoryFact removes one fact; DeleteMemoryEntity removes an
	// entity and its facts. Both return ErrNotFound for unknown IDs.
	DeleteMemoryFact(ctx context.Context, repo, id string) error
	DeleteMemoryEntity(ctx context.Context, repo, id string) error
	// MemoryStats summarises every repository that has memory, by repo name.
	MemoryStats(ctx context.Context) ([]memory.Stats, error)
	// MemoryEmbeddingTodo returns up to limit entities (any repository)
	// without an embedding made by model, oldest first.
	MemoryEmbeddingTodo(ctx context.Context, model string, limit int) ([]memory.Entity, error)
	// SetMemoryEmbeddings stores vectors made by model, by entity ID. All
	// vectors have the same length (1 to memory.MaxDimensions).
	SetMemoryEmbeddings(ctx context.Context, model string, vecs map[string][]float32) error
	// MemoryNearest returns up to k entities of repo embedded by model, most
	// similar to vec first, with memory.Similarity scores.
	MemoryNearest(ctx context.Context, repo, model string, vec []float32, k int) ([]memory.Scored, error)
}

// UsageStore keeps daily MCP usage counters (package usage).
type UsageStore interface {
	// AddUsage adds the buckets' counters to the stored ones (creating them).
	AddUsage(ctx context.Context, buckets []usage.Bucket) error
	// PruneUsage deletes buckets of days before beforeDay (YYYY-MM-DD).
	PruneUsage(ctx context.Context, beforeDay string) error
	// Usage returns buckets from sinceDay on, ordered by day, repo, tool, client.
	Usage(ctx context.Context, sinceDay string) ([]usage.Bucket, error)
}

// Store is a full read/write backend.
type Store interface {
	Reader
	Writer
	ReviewStore
	MemoryStore
	UsageStore
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
