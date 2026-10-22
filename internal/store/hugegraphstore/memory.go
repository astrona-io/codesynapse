package hugegraphstore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/vector"
)

// Agent memory: :mem_entity vertices joined by :fact edges (one edge per
// fact, the fact ID as sort key). The memory namespace ("org|graph") is the
// store's "repo"; its organisation is tenant_id. Every script starts at the
// tenant: g.V()/g.E() .has('tenant_id', tenant).has('ns', ns). What an upsert
// writes is computed by memory.Batch.Plan, shared with the other stores.

// E and F turn entities and facts into maps with memory's JSON names.
const memProlog = `def E = { v -> [id: v.value('eid'), repo: v.value('ns'), name: v.property('name').orElse(''), type: v.property('type').orElse(''),
  aliases: v.property('aliases').orElse([]), created_at: v.property('created_at').orElse(''), updated_at: v.property('updated_at').orElse('')] }
def F = { e -> [id: e.value('fid'), repo: e.value('ns'), subject: e.property('subj').orElse(''), predicate: e.property('predicate').orElse(''),
  object: e.property('obj').orElse(''), source: e.property('source').orElse(''), confirmations: e.property('confirmations').orElse(0L),
  confidence: e.property('confidence').orElse(0d), valid_from: e.property('valid_from').orElse(''), valid_to: e.property('valid_to').orElse(''),
  created_at: e.property('created_at').orElse(''), updated_at: e.property('updated_at').orElse('')] }
def ents = { -> g.V().hasLabel('mem_entity').has('tenant_id', tenant).has('ns', ns) }
def facts = { -> g.E().hasLabel('fact').has('tenant_id', tenant).has('ns', ns) }
`

const (
	// loadEntities returns every entity of the namespace (at most
	// memory.MaxEntitiesPerRepo) and the namespace's fact count.
	loadEntities = memProlog + `[[entities: ents().toList().collect { E(it) }, facts: facts().count().next()]]`
	// factsTouching returns facts with an endpoint in ids or an ID in fids,
	// current only unless history, most recent first.
	factsTouching = memProlog + `def want = ids as Set
def byID = fids as Set
def out = facts().toList().findAll { e ->
  (byID.contains(e.value('fid')) || want.contains(e.property('subj').orElse('')) || want.contains(e.property('obj').orElse(''))) &&
  (history || e.property('valid_to').orElse('') == '')
}
out.sort { a, b -> b.property('updated_at').orElse('') <=> a.property('updated_at').orElse('') ?: a.value('fid') <=> b.value('fid') }
out.take(limit as int).collect { F(it) }`
	// applyPlan deletes, then writes. Index lookups come first: HugeGraph
	// refuses them after writes in the same transaction.
	applyPlan = memProlog + `def delF = delFacts ? facts().has('fid', within(delFacts)).toList() : []
def delE = delEntities ? ents().toList().findAll { delEntities.contains(it.value('eid')) } : []
def put = { kv, k, v -> if (v != null && v != '' && v != []) { kv << k; kv << v } }
delF.each { it.remove() }
entities.each { e ->
  def old = g.V(e.v).tryNext()
  if (old.isPresent() && (old.get().value('tenant_id') != tenant || old.get().value('ns') != ns)) { throw new IllegalStateException('entity belongs to another namespace') }
  def kv = [T.label, 'mem_entity', T.id, e.v, 'eid', e.id, 'tenant_id', tenant, 'ns', ns]
  put(kv, 'name', e.name); put(kv, 'type', e.type); put(kv, 'aliases', e.aliases)
  put(kv, 'created_at', e.created_at); put(kv, 'updated_at', e.updated_at)
  if (old.isPresent()) { put(kv, 'emb_model', old.get().property('emb_model').orElse(null)) }
  graph.addVertex(kv as Object[])
}
factRows.each { f ->
  def kv = ['fid', f.id, 'tenant_id', tenant, 'ns', ns, 'subj', f.subject, 'obj', f.object, 'confirmations', f.confirmations, 'confidence', f.confidence]
  put(kv, 'predicate', f.predicate); put(kv, 'source', f.source); put(kv, 'valid_from', f.valid_from); put(kv, 'valid_to', f.valid_to)
  put(kv, 'created_at', f.created_at); put(kv, 'updated_at', f.updated_at)
  g.V(f.sv).next().addEdge('fact', g.V(f.ov).next(), kv as Object[])
}
delE.each { it.remove() }
delE.size()`
)

func entityVID(id string) string { return vid("e", id) }

// ns binds a namespace and its tenant.
func ns(namespace string) map[string]any {
	return map[string]any{"ns": namespace, "tenant": memory.TenantOf(namespace)}
}

func with(b map[string]any, kv ...any) map[string]any {
	for i := 0; i+1 < len(kv); i += 2 {
		b[kv[i].(string)] = kv[i+1]
	}
	return b
}

type loaded struct {
	Entities []memory.Entity `json:"entities"`
	Facts    int             `json:"facts"`
}

func (s *Store) loadEntities(ctx context.Context, namespace string) (loaded, error) {
	var res []loaded
	if err := s.read(ctx, loadEntities, ns(namespace), &res); err != nil || len(res) == 0 {
		return loaded{}, err
	}
	return res[0], nil
}

func (s *Store) factsTouching(ctx context.Context, namespace string, ids, fids []string, limit int, history bool) ([]memory.Fact, error) {
	out := []memory.Fact{}
	if len(ids) == 0 && len(fids) == 0 {
		return out, nil
	}
	var raw []json.RawMessage
	err := s.read(ctx, factsTouching, with(ns(namespace), "ids", nonNil(ids), "fids", nonNil(fids), "limit", limit, "history", history), &raw)
	if err != nil {
		return nil, err
	}
	for _, r := range raw {
		f, err := toFact(r)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

// toFact decodes an F map. Facts without confidence read as 1 and without
// valid_from as valid from their creation, as in the Neo4j store.
func toFact(raw json.RawMessage) (memory.Fact, error) {
	var f memory.Fact
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, err
	}
	if f.Confidence == 0 {
		f.Confidence = 1
	}
	if f.ValidFrom == "" {
		f.ValidFrom = f.CreatedAt
	}
	return f, nil
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// UpsertMemory implements store.MemoryStore. The read and the write are two
// requests; HugeGraph gives no transaction spanning both, so two writers on
// one namespace at the same moment may miss each other's aliases.
func (s *Store) UpsertMemory(ctx context.Context, b memory.Batch) error {
	cur, err := s.loadEntities(ctx, b.Repo)
	if err != nil {
		return err
	}
	names := b.Names()
	aliasOf := map[string]string{}
	byID := map[string]memory.Entity{}
	for _, e := range cur.Entities {
		byID[e.ID] = e
		for _, a := range e.Aliases {
			if slices.Contains(names, a) {
				aliasOf[a] = e.ID
			}
		}
	}
	b.Resolve(aliasOf)
	entityIDs, subjects, factIDs := b.Needs()
	st := memory.State{Entities: map[string]memory.Entity{}, Facts: map[string]memory.Fact{}, EntityCount: len(cur.Entities), FactCount: cur.Facts}
	for _, id := range entityIDs {
		if e, ok := byID[id]; ok {
			st.Entities[id] = e
		}
	}
	fs, err := s.factsTouching(ctx, b.Repo, subjects, factIDs, memory.MaxFactsPerRepo, true)
	if err != nil {
		return err
	}
	for _, f := range fs {
		st.Facts[f.ID] = f
	}
	plan, err := b.Plan(st)
	if err != nil {
		return err
	}
	entities := make([]map[string]any, len(plan.PutEntities))
	for i, e := range plan.PutEntities {
		entities[i] = map[string]any{"v": entityVID(e.ID), "id": e.ID, "name": e.Name, "type": e.Type, "aliases": nonNil(e.Aliases),
			"created_at": e.CreatedAt, "updated_at": e.UpdatedAt}
	}
	facts := make([]map[string]any, len(plan.PutFacts))
	for i, f := range plan.PutFacts {
		facts[i] = map[string]any{"id": f.ID, "sv": entityVID(f.Subject), "ov": entityVID(f.Object), "subject": f.Subject, "object": f.Object,
			"predicate": f.Predicate, "source": f.Source, "confirmations": int64(f.Confirmations), "confidence": f.Confidence,
			"valid_from": f.ValidFrom, "valid_to": f.ValidTo, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt}
	}
	err = s.write(ctx, applyPlan, with(ns(b.Repo), "delFacts", nonNil(plan.DeleteFacts), "delEntities", nonNil(plan.DeleteEntities),
		"entities", entities, "factRows", facts), nil)
	if err != nil {
		return err
	}
	return s.dropVectors(ctx, plan.DeleteEntities)
}

func (s *Store) dropVectors(ctx context.Context, ids []string) error {
	if s.vec == nil || len(ids) == 0 {
		return nil
	}
	return s.vec.Delete(ctx, ids)
}

// SearchMemory implements store.MemoryStore.
func (s *Store) SearchMemory(ctx context.Context, repo, query string, limit int, history bool) (memory.Graph, error) {
	limit = store.ClampLimit(limit, 20)
	g := memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
	cur, err := s.loadEntities(ctx, repo)
	if err != nil {
		return g, err
	}
	words := memory.Words(query)
	type scored struct {
		e     memory.Entity
		score int
	}
	var hits []scored
	for _, e := range cur.Entities {
		if sc := memory.Score(e, words); sc > 0 || len(words) == 0 {
			hits = append(hits, scored{e, sc})
		}
	}
	slices.SortFunc(hits, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(b.e.UpdatedAt, a.e.UpdatedAt), cmp.Compare(a.e.Name, b.e.Name))
	})
	matched := map[string]bool{}
	var ids []string
	for _, h := range hits[:min(len(hits), limit)] {
		matched[h.e.ID] = true
		ids = append(ids, h.e.ID)
		g.Entities = append(g.Entities, h.e)
	}
	if g.Facts, err = s.factsTouching(ctx, repo, ids, nil, min(limit*5, store.MaxLimit), history); err != nil {
		return g, err
	}
	g.Entities = append(g.Entities, endpoints(cur.Entities, g.Facts, matched)...)
	return g, nil
}

// MemorySubgraph implements store.MemoryStore.
func (s *Store) MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int, history bool) (memory.Graph, error) {
	limit = store.ClampLimit(limit, 50)
	g := memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
	cur, err := s.loadEntities(ctx, repo)
	if err != nil {
		return g, err
	}
	known := map[string]bool{}
	for _, e := range cur.Entities {
		known[e.ID] = true
	}
	reached := map[string]bool{}
	for _, id := range ids {
		if known[id] {
			reached[id] = true
		}
	}
	if min(max(depth, 1), 2) == 2 {
		fs, err := s.factsTouching(ctx, repo, keys(reached), nil, memory.MaxFactsPerRepo, history)
		if err != nil {
			return g, err
		}
		for _, f := range fs {
			reached[f.Subject], reached[f.Object] = true, true
		}
	}
	if g.Facts, err = s.factsTouching(ctx, repo, keys(reached), nil, limit, history); err != nil {
		return g, err
	}
	g.Entities = endpoints(cur.Entities, g.Facts, nil)
	return g, nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// endpoints returns the entities facts refer to, except those in skip, by name.
func endpoints(all []memory.Entity, facts []memory.Fact, skip map[string]bool) []memory.Entity {
	want := map[string]bool{}
	for _, f := range facts {
		want[f.Subject], want[f.Object] = true, true
	}
	var out []memory.Entity
	for _, e := range all {
		if want[e.ID] && !skip[e.ID] {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b memory.Entity) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out
}

const (
	deleteFact = memProlog + `def es = facts().has('fid', target).toList()
es.each { it.remove() }
es.size()`
	deleteEntity = memProlog + `def vs = ents().toList().findAll { it.value('eid') == target }
vs.each { it.remove() }
vs.size()`
)

// DeleteMemoryFact implements store.MemoryStore.
func (s *Store) DeleteMemoryFact(ctx context.Context, repo, id string) error {
	var n []int
	if err := s.write(ctx, deleteFact, with(ns(repo), "target", id), &n); err != nil {
		return err
	}
	if len(n) == 0 || n[0] == 0 {
		return fmt.Errorf("fact %q: %w", id, store.ErrNotFound)
	}
	return nil
}

// DeleteMemoryEntity implements store.MemoryStore. Removing the vertex
// removes its facts.
func (s *Store) DeleteMemoryEntity(ctx context.Context, repo, id string) error {
	var n []int
	if err := s.write(ctx, deleteEntity, with(ns(repo), "target", id), &n); err != nil {
		return err
	}
	if len(n) == 0 || n[0] == 0 {
		return fmt.Errorf("entity %q: %w", id, store.ErrNotFound)
	}
	return s.dropVectors(ctx, []string{id})
}

// memoryStats spans every namespace: it serves the platform statistics
// and runs without a tenant; callers filter by organisation.
const memoryStats = `def by = [:]
def names = [:]
def row = { k -> def r = by[k]; if (r == null) { r = [repo: k, entities: 0L, facts: 0L, history: 0L, confirmations: 0L, fact_chars: 0L, sources: [:], last_updated: '']; by[k] = r }; r }
g.V().hasLabel('mem_entity').toList().each { v ->
  def r = row(v.value('ns')); r.entities += 1L
  names[v.value('eid')] = v.property('name').orElse('')
  def u = v.property('updated_at').orElse(''); if (u > r.last_updated) { r.last_updated = u }
}
g.E().hasLabel('fact').toList().each { e ->
  def r = row(e.value('ns'))
  if (e.property('valid_to').orElse('') != '') { r.history += 1L; return }
  r.facts += 1L
  r.confirmations += e.property('confirmations').orElse(0L)
  r.fact_chars += (names[e.property('subj').orElse('')] ?: '').length() + e.property('predicate').orElse('').length() + (names[e.property('obj').orElse('')] ?: '').length()
  def src = e.property('source').orElse(''); r.sources[src] = (r.sources[src] ?: 0L) + 1L
  def u = e.property('updated_at').orElse(''); if (u > r.last_updated) { r.last_updated = u }
}
by.values().findAll { it.entities > 0 || it.facts > 0 || it.history > 0 }.sort { it.repo }`

// MemoryStats implements store.MemoryStore.
func (s *Store) MemoryStats(ctx context.Context) ([]memory.Stats, error) {
	out := []memory.Stats{}
	err := s.read(ctx, memoryStats, nil, &out)
	return out, err
}

const (
	embeddingTodo = `def out = g.V().hasLabel('mem_entity').toList().findAll { it.property('emb_model').orElse('') != model }
out.sort { a, b -> a.property('updated_at').orElse('') <=> b.property('updated_at').orElse('') ?: a.value('eid') <=> b.value('eid') }
out.take(limit as int).collect { v -> [id: v.value('eid'), repo: v.value('ns'), name: v.property('name').orElse(''), type: v.property('type').orElse(''),
  aliases: v.property('aliases').orElse([]), created_at: v.property('created_at').orElse(''), updated_at: v.property('updated_at').orElse('')] }`
	entityHomes  = `vs.collect { g.V(it).tryNext() }.findAll { it.isPresent() }.collect { def v = it.get(); [v.value('eid'), v.value('tenant_id'), v.value('ns')] }`
	markEmbedded = `vs.each { id -> g.V(id).tryNext().ifPresent { it.property('emb_model', model) } }
vs.size()`
)

// MemoryEmbeddingTodo implements store.MemoryStore. Without a vector index
// there is nowhere to keep vectors, so there is nothing to do.
func (s *Store) MemoryEmbeddingTodo(ctx context.Context, model string, limit int) ([]memory.Entity, error) {
	if s.vec == nil {
		return nil, nil
	}
	var out []memory.Entity
	err := s.read(ctx, embeddingTodo, map[string]any{"model": model, "limit": store.ClampLimit(limit, 64)}, &out)
	return out, err
}

// SetMemoryEmbeddings implements store.MemoryStore: vectors go to the
// vector index with each entity's tenant and namespace, then the entities
// are marked as embedded by model.
func (s *Store) SetMemoryEmbeddings(ctx context.Context, model string, vecs map[string][]float32) error {
	if err := memory.CheckVectors(vecs); err != nil || len(vecs) == 0 || s.vec == nil {
		return err
	}
	vids := make([]string, 0, len(vecs))
	for id := range vecs {
		vids = append(vids, entityVID(id))
	}
	var homes [][]string
	if err := s.read(ctx, entityHomes, map[string]any{"vs": vids}, &homes); err != nil {
		return err
	}
	items := make([]vector.Item, 0, len(homes))
	marked := make([]string, 0, len(homes))
	for _, h := range homes {
		if len(h) == 3 {
			items = append(items, vector.Item{ID: h[0], Tenant: h[1], Namespace: h[2], Vector: vecs[h[0]]})
			marked = append(marked, entityVID(h[0]))
		}
	}
	if err := s.vec.Upsert(ctx, model, items); err != nil {
		return fmt.Errorf("vector index: %w", err)
	}
	return s.write(ctx, markEmbedded, map[string]any{"vs": marked, "model": model}, nil)
}

// MemoryNearest implements store.MemoryStore. The index searches within the
// namespace's tenant; hits are then loaded through the tenant-rooted entity
// lookup, so a vector whose entity is gone or elsewhere is dropped.
func (s *Store) MemoryNearest(ctx context.Context, repo, model string, vec []float32, k int) ([]memory.Scored, error) {
	out := []memory.Scored{}
	if s.vec == nil {
		return out, nil
	}
	k = vector.ClampK(k)
	hits, err := s.vec.Search(ctx, model, memory.TenantOf(repo), repo, vec, k)
	if err != nil || len(hits) == 0 {
		return out, err
	}
	cur, err := s.loadEntities(ctx, repo)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]memory.Entity, len(cur.Entities))
	for _, e := range cur.Entities {
		byID[e.ID] = e
	}
	for _, h := range hits {
		if e, ok := byID[h.ID]; ok {
			out = append(out, memory.Scored{Entity: e, Score: h.Score})
		}
	}
	return out[:min(len(out), k)], nil
}
