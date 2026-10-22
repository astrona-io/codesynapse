// Package sessionmemory keeps chat facts separate from durable repository memory.
package sessionmemory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
)

// Limits bound each persisted chat scope and the tenant aggregate.
const (
	MaxFacts         = 512
	MaxEntities      = 1024
	MaxNamespaces    = 16
	MaxTenantScopes  = 2048
	MaxSessionFacts  = 2048
	MaxTenantFacts   = 65536
	MaxDocumentBytes = 24 << 10
	Retention        = 7 * 24 * time.Hour
)

// Scope, lifetime, optimistic concurrency and quota failures fail closed.
var (
	ErrScope    = errors.New("session memory scope does not match execution context")
	ErrClosed   = errors.New("session memory is deleted or expired")
	ErrConflict = errors.New("session memory changed concurrently; retry")
	ErrLimit    = errors.New("session memory capacity exceeded")
)

// Scope is supplied by authenticated middleware, never by model arguments.
type Scope struct {
	TenantID  string `json:"tenant_id"`
	SessionID string `json:"session_id"`
	Namespace string `json:"namespace"`
}

// Check validates the content namespace against inherited execution identity.
func (s Scope) Check(ctx context.Context) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if id.TenantID != s.TenantID || id.SessionID != s.SessionID || len(s.Namespace) > 1024 ||
		!strings.HasPrefix(s.Namespace, s.TenantID+"|") || len(s.Namespace) <= len(s.TenantID)+1 {
		return ErrScope
	}
	return ctx.Err()
}

// Key identifies storage records without replacing the runtime session ID.
func (s Scope) Key() string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return "session:" + hex.EncodeToString(h[:])
}

// Document is the bounded graph snapshot committed atomically by adapters.
type Document struct {
	Scope     Scope        `json:"scope"`
	Revision  int64        `json:"revision"`
	ExpiresAt time.Time    `json:"expires_at"`
	Deleted   bool         `json:"deleted"`
	Graph     memory.Graph `json:"graph"`
}

// Backend keeps optimistic revisions and tenant quotas inside its transaction.
type Backend interface {
	LoadSessionDocument(context.Context, Scope) (Document, error)
	CommitSessionDocument(context.Context, Document, int64) error
	RevokeSessionDocuments(context.Context, string, string) error
}

// Engine shares validation and graph semantics across the three stores.
type Engine struct{ Backend Backend }

// UpsertSessionMemory validates and atomically merges transient chat facts.
func (e Engine) UpsertSessionMemory(ctx context.Context, scope Scope, triples []memory.Triple, source string, expiresAt time.Time) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	for range 3 {
		d, err := e.Backend.LoadSessionDocument(ctx, scope)
		if err != nil {
			return err
		}
		old := d.Revision
		d, err = Apply(d, scope, triples, source, expiresAt, time.Now())
		if err != nil {
			return err
		}
		if err = e.Backend.CommitSessionDocument(ctx, d, old); !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}

// ReadSessionMemory returns scoped facts and expires retained data on access.
func (e Engine) ReadSessionMemory(ctx context.Context, scope Scope, query string, limit int) (memory.Graph, error) {
	if err := scope.Check(ctx); err != nil {
		return memory.Graph{}, err
	}
	if len(query) > 4096 {
		return memory.Graph{}, ErrLimit
	}
	d, err := e.Backend.LoadSessionDocument(ctx, scope)
	if err != nil {
		return memory.Graph{}, err
	}
	if err := CheckDocument(d, scope, time.Now()); err != nil {
		if errors.Is(err, ErrClosed) && !d.Deleted {
			d.Deleted = true
			d.Graph = EmptyGraph()
			old := d.Revision
			d.Revision++
			if clean := e.Backend.CommitSessionDocument(ctx, d, old); clean != nil {
				return memory.Graph{}, clean
			}
		}
		return memory.Graph{}, err
	}
	return Select(d.Graph, query, limit), nil
}

// DeleteSessionMemory removes one namespace and retains its replay tombstone.
func (e Engine) DeleteSessionMemory(ctx context.Context, scope Scope) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	for range 3 {
		d, err := e.Backend.LoadSessionDocument(ctx, scope)
		if err != nil {
			return err
		}
		old := d.Revision
		d.Scope, d.Deleted, d.Graph = scope, true, EmptyGraph()
		d.Revision++
		if err = e.Backend.CommitSessionDocument(ctx, d, old); !errors.Is(err, ErrConflict) {
			return err
		}
	}
	return ErrConflict
}

// DeleteExecutionMemory revokes every namespace of the selected execution.
func (e Engine) DeleteExecutionMemory(ctx context.Context, tenant, session string) error {
	if err := CheckExecution(ctx, tenant, session); err != nil {
		return err
	}
	return e.Backend.RevokeSessionDocuments(ctx, tenant, session)
}

// CheckExecution requires the authenticated tenant and inherited primary session.
func CheckExecution(ctx context.Context, tenant, session string) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if tenant != id.TenantID || session != id.SessionID {
		return ErrScope
	}
	return ctx.Err()
}

// EmptyGraph returns an empty response with the mandatory memory notice.
func EmptyGraph() memory.Graph {
	return memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
}

// CheckDocument rejects foreign endpoints, expired snapshots and invalid facts.
func CheckDocument(d Document, scope Scope, now time.Time) error {
	if d.Revision == 0 {
		if d.Scope != (Scope{}) || d.Deleted || len(d.Graph.Entities) > 0 || len(d.Graph.Facts) > 0 {
			return ErrScope
		}
		return nil
	}
	if d.Scope != scope {
		return ErrScope
	}
	if d.Deleted || !now.Before(d.ExpiresAt) {
		return ErrClosed
	}
	if len(d.Graph.Entities) > MaxEntities || len(d.Graph.Facts) > MaxFacts {
		return ErrLimit
	}
	ids := map[string]bool{}
	for _, ent := range d.Graph.Entities {
		if ent.Repo != scope.Key() || ent.ID != memory.EntityID(scope.Key(), ent.Name) {
			return ErrScope
		}
		ids[ent.ID] = true
	}
	for _, f := range d.Graph.Facts {
		if f.Repo != scope.Key() || !ids[f.Subject] || !ids[f.Object] || !memory.ValidPredicate(f.Predicate) || f.ID != memory.FactID(f.Subject, f.Predicate, f.Object) {
			return ErrScope
		}
	}
	return nil
}

// CheckCommit rejects malformed adapter writes even if the engine is bypassed.
func CheckCommit(d Document, expected int64) error {
	if expected < 0 || d.Revision != expected+1 {
		return ErrConflict
	}
	if d.Deleted {
		if len(d.Graph.Entities) > 0 || len(d.Graph.Facts) > 0 {
			return ErrScope
		}
		return nil
	}
	if err := CheckDocument(d, d.Scope, time.Now()); err != nil {
		return err
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(b) > MaxDocumentBytes {
		return ErrLimit
	}
	return nil
}

// CheckTransition prevents adapter callers from reviving tombstones or extending retention.
func CheckTransition(old, next Document, now time.Time) error {
	if old.Revision == 0 {
		return nil
	}
	if old.Scope != next.Scope {
		return ErrScope
	}
	if !old.ExpiresAt.Equal(next.ExpiresAt) {
		return ErrClosed
	}
	if !next.Deleted && (old.Deleted || !now.Before(old.ExpiresAt)) {
		return ErrClosed
	}
	return nil
}

// Apply builds a bounded new revision without extending its initial retention.
func Apply(d Document, scope Scope, triples []memory.Triple, source string, expiresAt, now time.Time) (Document, error) {
	if err := CheckDocument(d, scope, now); err != nil {
		return Document{}, err
	}
	if d.Revision == 0 {
		if expiresAt.IsZero() {
			expiresAt = now.Add(Retention)
		}
		if !expiresAt.After(now) || expiresAt.After(now.Add(Retention)) {
			return Document{}, ErrClosed
		}
		d = Document{Scope: scope, ExpiresAt: expiresAt, Graph: EmptyGraph()}
	}
	b, err := memory.Prepare(scope.Key(), triples, source, now)
	if err != nil {
		return Document{}, err
	}
	state := memory.State{Entities: map[string]memory.Entity{}, Facts: map[string]memory.Fact{}, EntityCount: len(d.Graph.Entities), FactCount: len(d.Graph.Facts)}
	aliases := map[string]string{}
	for _, ent := range d.Graph.Entities {
		state.Entities[ent.ID] = ent
		for _, a := range ent.Aliases {
			aliases[a] = ent.ID
		}
	}
	for _, f := range d.Graph.Facts {
		state.Facts[f.ID] = f
	}
	for i := range b.Facts {
		b.Facts[i].OriginSessionID = scope.SessionID
	}
	b.Resolve(aliases)
	p, err := b.Plan(state)
	if err != nil {
		return Document{}, err
	}
	for _, id := range p.DeleteEntities {
		delete(state.Entities, id)
	}
	for _, id := range p.DeleteFacts {
		delete(state.Facts, id)
	}
	for _, ent := range p.PutEntities {
		state.Entities[ent.ID] = ent
	}
	for _, f := range p.PutFacts {
		state.Facts[f.ID] = f
	}
	if len(state.Entities) > MaxEntities || len(state.Facts) > MaxFacts {
		return Document{}, ErrLimit
	}
	d.Graph = EmptyGraph()
	for _, ent := range state.Entities {
		d.Graph.Entities = append(d.Graph.Entities, ent)
	}
	for _, f := range state.Facts {
		d.Graph.Facts = append(d.Graph.Facts, f)
	}
	slices.SortFunc(d.Graph.Entities, func(a, b memory.Entity) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(d.Graph.Facts, func(a, b memory.Fact) int { return strings.Compare(a.ID, b.ID) })
	d.Revision++
	encoded, err := json.Marshal(d)
	if err != nil {
		return Document{}, err
	}
	if len(encoded) > MaxDocumentBytes {
		return Document{}, ErrLimit
	}
	return d, nil
}

// Select ranks scoped names and includes their current fact endpoints.
func Select(g memory.Graph, query string, limit int) memory.Graph {
	limit = min(max(limit, 1), MaxFacts)
	out := EmptyGraph()
	want := map[string]bool{}
	if strings.TrimSpace(query) == "" {
		for _, e := range g.Entities {
			want[e.ID] = true
		}
	} else {
		for _, hit := range memory.RankBM25(g.Entities, query, min(limit, 8)) {
			want[hit.ID] = true
		}
	}
	keep := map[string]bool{}
	for id := range want {
		keep[id] = true
	}
	for _, f := range g.Facts {
		if f.Current() && (want[f.Subject] || want[f.Object]) && len(out.Facts) < limit {
			out.Facts = append(out.Facts, f)
			keep[f.Subject], keep[f.Object] = true, true
		}
	}
	for _, ent := range g.Entities {
		if keep[ent.ID] {
			out.Entities = append(out.Entities, ent)
		}
	}
	return out
}

// Quota is projected from scope roots; graph data need not be scanned for caps.
type Quota struct {
	Scope     Scope
	Facts     int
	ExpiresAt time.Time
	Deleted   bool
}

// CheckQuota bounds retained scope roots and live facts per session and tenant.
func CheckQuota(rows []Quota, next Document, now time.Time) error {
	count, ns, sessionFacts, tenantFacts := 1, 1, len(next.Graph.Facts), len(next.Graph.Facts)
	for _, row := range rows {
		if row.Scope.TenantID != next.Scope.TenantID || row.Scope == next.Scope {
			continue
		}
		count++
		if row.Scope.SessionID == next.Scope.SessionID {
			ns++
		}
		if row.Deleted || !now.Before(row.ExpiresAt) {
			continue
		}
		tenantFacts += row.Facts
		if row.Scope.SessionID == next.Scope.SessionID {
			sessionFacts += row.Facts
		}
	}
	if count > MaxTenantScopes || ns > MaxNamespaces {
		return ErrLimit
	}
	if !next.Deleted && (sessionFacts > MaxSessionFacts || tenantFacts > MaxTenantFacts) {
		return ErrLimit
	}
	return nil
}
