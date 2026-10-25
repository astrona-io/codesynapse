package nebulastore

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/vector"
)

var _ store.MemoryStore = (*Store)(nil)

// Agent memory: :mem_entity and :mem_fact vertices. A fact is a vertex, not
// an edge, because its ID would have to be the edge rank and ranks are
// numbers; subj and obj name the entity IDs. The memory namespace ("org|graph")
// is the store's "repo", its organisation is tenant_id, and every statement
// over memory starts at has tenant_id and ns. What an upsert writes is computed
// by memory.Batch.Plan, shared with the other stores.

// scanMax bounds a scan over every namespace (statistics, enumeration).
const scanMax = 200000

const (
	entitiesOf = "MATCH (v:`mem_entity`) WHERE v.`mem_entity`.`tenant_id` == $tenant AND v.`mem_entity`.`ns` == $ns RETURN properties(v) AS p LIMIT $limit"
	factsOf    = "MATCH (v:`mem_fact`) WHERE v.`mem_fact`.`tenant_id` == $tenant AND v.`mem_fact`.`ns` == $ns RETURN properties(v) AS p LIMIT $limit"
)

func entityVID(id string) string { return vid("e", id) }

// factVID is per namespace: a fact ID only has to be unique inside one.
func factVID(namespace, id string) string { return vid("f", namespace+"|"+id) }

func nsArgs(namespace string, limit int) map[string]any {
	return map[string]any{"ns": namespace, "tenant": memory.TenantOf(namespace), "limit": int64(limit)}
}

func str(p map[string]any, k string) string { v, _ := p[k].(string); return v }

func toEntity(p map[string]any) (memory.Entity, error) {
	e := memory.Entity{ID: str(p, "eid"), Repo: str(p, "ns"), Name: str(p, "name"), Type: str(p, "type"), CreatedAt: str(p, "created_at"), UpdatedAt: str(p, "updated_at")}
	if a := str(p, "aliases"); a != "" {
		if err := json.Unmarshal([]byte(a), &e.Aliases); err != nil {
			return e, fmt.Errorf("entity %q aliases: %w", e.ID, err)
		}
	}
	return e, nil
}

// toFact reads a fact; as in the memory store, a missing confidence reads as
// 1 and a missing valid_from as the creation time.
func toFact(p map[string]any) memory.Fact {
	f := memory.Fact{ID: str(p, "fid"), Repo: str(p, "ns"), Subject: str(p, "subj"), Object: str(p, "obj"), Predicate: str(p, "predicate"), Source: str(p, "source"),
		OriginSessionID: str(p, "origin_session_id"), ValidFrom: str(p, "valid_from"), ValidTo: str(p, "valid_to"), CreatedAt: str(p, "created_at"), UpdatedAt: str(p, "updated_at")}
	switch n := p["confirmations"].(type) {
	case int64:
		f.Confirmations = int(n)
	}
	if c, ok := p["confidence"].(float64); ok {
		f.Confidence = c
	}
	if f.Confidence == 0 {
		f.Confidence = 1
	}
	if f.ValidFrom == "" {
		f.ValidFrom = f.CreatedAt
	}
	return f
}

func (s *Store) loadEntities(ctx context.Context, namespace string) ([]memory.Entity, error) {
	rows, err := s.read(ctx, entitiesOf, nsArgs(namespace, memory.MaxEntitiesPerRepo))
	if err != nil {
		return nil, err
	}
	out := make([]memory.Entity, 0, len(rows))
	for _, r := range rows {
		p, _ := r["p"].(map[string]any)
		e, err := toEntity(p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b memory.Entity) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// loadFacts returns every fact of the namespace, history included.
func (s *Store) loadFacts(ctx context.Context, namespace string) ([]memory.Fact, error) {
	rows, err := s.read(ctx, factsOf, nsArgs(namespace, memory.MaxFactsPerRepo+1))
	if err != nil {
		return nil, err
	}
	out := make([]memory.Fact, 0, len(rows))
	for _, r := range rows {
		p, _ := r["p"].(map[string]any)
		out = append(out, toFact(p))
	}
	return out, nil
}

// touching picks facts with an endpoint in ids or an ID in fids, current
// only unless history, most recent first, at most limit.
func touching(all []memory.Fact, ids, fids []string, limit int, history bool) []memory.Fact {
	want, byID := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, id := range fids {
		byID[id] = true
	}
	out := []memory.Fact{}
	for _, f := range all {
		if (byID[f.ID] || want[f.Subject] || want[f.Object]) && (history || f.ValidTo == "") {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b memory.Fact) int {
		return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), limit)]
}

// UpsertMemory implements store.MemoryStore. The read and the writes are
// separate requests; NebulaGraph has no transaction around them, so two
// writers on one namespace at the same moment may miss each other's aliases,
// and a failure between the writes leaves part of the batch applied (the
// same batch can be sent again: every write is an upsert or a delete).
func (s *Store) UpsertMemory(ctx context.Context, b memory.Batch) error {
	b = b.StampOrigin(ctx)
	entities, err := s.loadEntities(ctx, b.Repo)
	if err != nil {
		return err
	}
	all, err := s.loadFacts(ctx, b.Repo)
	if err != nil {
		return err
	}
	names := b.Names()
	aliasOf := map[string]string{}
	byID := map[string]memory.Entity{}
	for _, e := range entities {
		byID[e.ID] = e
		for _, a := range e.Aliases {
			if slices.Contains(names, a) {
				aliasOf[a] = e.ID
			}
		}
	}
	b.Resolve(aliasOf)
	entityIDs, subjects, factIDs := b.Needs()
	st := memory.State{Entities: map[string]memory.Entity{}, Facts: map[string]memory.Fact{}, EntityCount: len(entities), FactCount: len(all)}
	for _, id := range entityIDs {
		if e, ok := byID[id]; ok {
			st.Entities[id] = e
		}
	}
	for _, f := range touching(all, subjects, factIDs, memory.MaxFactsPerRepo, true) {
		st.Facts[f.ID] = f
	}
	plan, err := b.Plan(st)
	if err != nil {
		return err
	}
	if err := s.ownEntities(ctx, b.Repo, plan.PutEntities); err != nil {
		return err
	}
	// Facts of removed entities go with them, as edges did in the graph.
	gone := map[string]bool{}
	for _, id := range plan.DeleteEntities {
		gone[id] = true
	}
	dropFacts := slices.Clone(plan.DeleteFacts)
	for _, f := range all {
		if gone[f.Subject] || gone[f.Object] {
			dropFacts = append(dropFacts, f.ID)
		}
	}
	if err := s.deleteVertices(ctx, mapSlice(dropFacts, func(id string) string { return factVID(b.Repo, id) })); err != nil {
		return err
	}
	for _, e := range plan.PutEntities {
		aliases, _ := json.Marshal(nonNil(e.Aliases)) //nolint:errcheck // a []string always marshals
		// UPSERT leaves emb_model alone, so a rewritten entity keeps its marker.
		if _, err := s.write(ctx, "UPSERT VERTEX ON `mem_entity` "+literal(entityVID(e.ID))+" SET `eid` = $id, `tenant_id` = $tenant, `ns` = $ns, `name` = $name, `type` = $type,"+
			" `aliases` = $aliases, `created_at` = $created, `updated_at` = $updated",
			map[string]any{"id": e.ID, "tenant": memory.TenantOf(b.Repo), "ns": b.Repo, "name": e.Name, "type": e.Type, "aliases": string(aliases), "created": e.CreatedAt, "updated": e.UpdatedAt}); err != nil {
			return err
		}
	}
	for off := 0; off < len(plan.PutFacts); off += vertexBatch {
		chunk := plan.PutFacts[off:min(off+vertexBatch, len(plan.PutFacts))]
		params := map[string]any{"tenant": memory.TenantOf(b.Repo), "ns": b.Repo}
		values := make([]string, len(chunk))
		for i, f := range chunk {
			k := func(name string) string { n := fmt.Sprintf("%s%d", name, i); return "$" + n }
			for name, v := range map[string]any{"id": f.ID, "subj": f.Subject, "obj": f.Object, "pred": f.Predicate, "src": f.Source, "origin": f.OriginSessionID,
				"from": f.ValidFrom, "to": f.ValidTo, "created": f.CreatedAt, "updated": f.UpdatedAt, "conf": int64(f.Confirmations), "score": f.Confidence} {
				params[fmt.Sprintf("%s%d", name, i)] = v
			}
			values[i] = literal(factVID(b.Repo, f.ID)) + ":(" + strings.Join([]string{k("id"), "$tenant", "$ns", k("subj"), k("obj"), k("pred"), k("src"), k("origin"),
				k("from"), k("to"), k("created"), k("updated"), k("conf"), k("score")}, ",") + ")"
		}
		if _, err := s.write(ctx, "INSERT VERTEX `mem_fact` (`fid`,`tenant_id`,`ns`,`subj`,`obj`,`predicate`,`source`,`origin_session_id`,`valid_from`,`valid_to`,`created_at`,`updated_at`,`confirmations`,`confidence`) VALUES "+
			strings.Join(values, ","), params); err != nil {
			return err
		}
	}
	if err := s.deleteVertices(ctx, mapSlice(plan.DeleteEntities, entityVID)); err != nil {
		return err
	}
	return s.dropVectors(ctx, plan.DeleteEntities)
}

// ownEntities refuses to overwrite an entity that exists in another namespace.
func (s *Store) ownEntities(ctx context.Context, namespace string, put []memory.Entity) error {
	if len(put) == 0 {
		return nil
	}
	rows, err := s.read(ctx, "FETCH PROP ON `mem_entity` "+literals(mapSlice(put, func(e memory.Entity) string { return entityVID(e.ID) }))+
		" YIELD properties(vertex).tenant_id AS tenant, properties(vertex).ns AS ns", nil)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r["ns"] != namespace || r["tenant"] != memory.TenantOf(namespace) {
			return fmt.Errorf("entity belongs to another namespace")
		}
	}
	return nil
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func (s *Store) deleteVertices(ctx context.Context, vids []string) error {
	for off := 0; off < len(vids); off += deleteBatch {
		if _, err := s.write(ctx, "DELETE VERTEX "+literals(vids[off:min(off+deleteBatch, len(vids))]), nil); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
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
	entities, err := s.loadEntities(ctx, repo)
	if err != nil {
		return g, err
	}
	words := memory.Words(query)
	type scored struct {
		e     memory.Entity
		score int
	}
	var hits []scored
	for _, e := range entities {
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
	all, err := s.loadFacts(ctx, repo)
	if err != nil {
		return g, err
	}
	g.Facts = touching(all, ids, nil, min(limit*5, store.MaxLimit), history)
	g.Entities = append(g.Entities, endpoints(entities, g.Facts, matched)...)
	return g, nil
}

// MemorySubgraph implements store.MemoryStore.
func (s *Store) MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int, history bool) (memory.Graph, error) {
	if len(ids) > store.MaxLimit {
		return memory.Graph{}, fmt.Errorf("memory roots exceed %d", store.MaxLimit)
	}
	limit = store.ClampLimit(limit, 50)
	g := memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
	entities, err := s.loadEntities(ctx, repo)
	if err != nil {
		return g, err
	}
	all, err := s.loadFacts(ctx, repo)
	if err != nil {
		return g, err
	}
	known := map[string]bool{}
	for _, e := range entities {
		known[e.ID] = true
	}
	reached := map[string]bool{}
	for _, id := range ids {
		if known[id] {
			reached[id] = true
		}
	}
	if min(max(depth, 1), 2) == 2 {
		for _, f := range touching(all, keys(reached), nil, memory.MaxFactsPerRepo, history) {
			reached[f.Subject], reached[f.Object] = true, true
		}
	}
	g.Facts = touching(all, keys(reached), nil, limit, history)
	g.Entities = endpoints(entities, g.Facts, nil)
	seen := map[string]bool{}
	for _, e := range g.Entities {
		seen[e.ID] = true
	}
	for _, e := range entities {
		if slices.Contains(ids, e.ID) && !seen[e.ID] {
			g.Entities = append(g.Entities, e)
		}
	}
	slices.SortFunc(g.Entities, func(a, b memory.Entity) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
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

// owned reports whether the vertex exists in the namespace of repo.
func (s *Store) owned(ctx context.Context, tag, vertex, repo string) (bool, error) {
	rows, err := s.read(ctx, "FETCH PROP ON "+quote(tag)+" "+literal(vertex)+" YIELD properties(vertex).tenant_id AS tenant, properties(vertex).ns AS ns", nil)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	return rows[0]["ns"] == repo && rows[0]["tenant"] == memory.TenantOf(repo), nil
}

// DeleteMemoryFact implements store.MemoryStore.
func (s *Store) DeleteMemoryFact(ctx context.Context, repo, id string) error {
	v := factVID(repo, id)
	ok, err := s.owned(ctx, "mem_fact", v, repo)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("fact %q: %w", id, store.ErrNotFound)
	}
	return s.deleteVertices(ctx, []string{v})
}

// DeleteMemoryEntity implements store.MemoryStore. Its facts go with it.
func (s *Store) DeleteMemoryEntity(ctx context.Context, repo, id string) error {
	ok, err := s.owned(ctx, "mem_entity", entityVID(id), repo)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("entity %q: %w", id, store.ErrNotFound)
	}
	all, err := s.loadFacts(ctx, repo)
	if err != nil {
		return err
	}
	var vids []string
	for _, f := range all {
		if f.Subject == id || f.Object == id {
			vids = append(vids, factVID(repo, f.ID))
		}
	}
	if err := s.deleteVertices(ctx, vids); err != nil {
		return err
	}
	if err := s.deleteVertices(ctx, []string{entityVID(id)}); err != nil {
		return err
	}
	return s.dropVectors(ctx, []string{id})
}

// MemoryStats implements store.MemoryStore. It spans every namespace (the
// platform statistics run without a tenant; callers filter by organisation).
func (s *Store) MemoryStats(ctx context.Context) ([]memory.Stats, error) {
	ents, err := s.read(ctx, "MATCH (v:`mem_entity`) RETURN properties(v) AS p LIMIT $limit", map[string]any{"limit": int64(scanMax)})
	if err != nil {
		return nil, err
	}
	facts, err := s.read(ctx, "MATCH (v:`mem_fact`) RETURN properties(v) AS p LIMIT $limit", map[string]any{"limit": int64(scanMax)})
	if err != nil {
		return nil, err
	}
	by := map[string]*memory.Stats{}
	names := map[string]string{}
	row := func(k string) *memory.Stats {
		r := by[k]
		if r == nil {
			r = &memory.Stats{Repo: k, Sources: map[string]int{}}
			by[k] = r
		}
		return r
	}
	for _, r := range ents {
		p, _ := r["p"].(map[string]any)
		st := row(str(p, "ns"))
		st.Entities++
		names[str(p, "eid")] = str(p, "name")
		if u := str(p, "updated_at"); u > st.LastUpdated {
			st.LastUpdated = u
		}
	}
	for _, r := range facts {
		p, _ := r["p"].(map[string]any)
		f := toFact(p)
		st := row(f.Repo)
		if f.ValidTo != "" {
			st.History++
			continue
		}
		st.Facts++
		st.Confirmations += f.Confirmations
		st.FactChars += len(names[f.Subject]) + len(f.Predicate) + len(names[f.Object])
		st.Sources[f.Source]++
		if f.UpdatedAt > st.LastUpdated {
			st.LastUpdated = f.UpdatedAt
		}
	}
	out := []memory.Stats{}
	for _, st := range by {
		out = append(out, *st)
	}
	slices.SortFunc(out, func(a, b memory.Stats) int { return cmp.Compare(a.Repo, b.Repo) })
	return out, nil
}

// MemoryFactCounts implements store.MemoryStore. This operator-only
// enumeration returns namespace IDs, never memory content; each count then
// starts from that namespace's tenant and namespace.
func (s *Store) MemoryFactCounts(ctx context.Context, afterNamespace string, limit int) ([]store.MemoryFactCount, error) {
	rows, err := s.read(ctx, "MATCH (v:`mem_entity`) WHERE v.`mem_entity`.`ns` > $after RETURN DISTINCT v.`mem_entity`.`ns` AS ns ORDER BY ns LIMIT $limit",
		map[string]any{"after": afterNamespace, "limit": int64(store.ClampLimit(limit, 100))})
	if err != nil {
		return nil, err
	}
	out := make([]store.MemoryFactCount, 0, len(rows))
	for _, r := range rows {
		namespace, _ := r["ns"].(string)
		n, err := s.read(ctx, "MATCH (v:`mem_fact`) WHERE v.`mem_fact`.`tenant_id` == $tenant AND v.`mem_fact`.`ns` == $ns AND v.`mem_fact`.`valid_to` == \"\" RETURN count(v) AS n",
			map[string]any{"tenant": memory.TenantOf(namespace), "ns": namespace})
		if err != nil {
			return nil, err
		}
		c := store.MemoryFactCount{Namespace: namespace}
		if len(n) == 1 {
			c.Facts, _ = n[0]["n"].(int64)
		}
		out = append(out, c)
	}
	return out, nil
}

// MemoryEmbeddingTodo implements store.MemoryStore. Without a vector index
// there is nowhere to keep vectors, so there is nothing to do.
func (s *Store) MemoryEmbeddingTodo(ctx context.Context, model string, limit int) ([]memory.Entity, error) {
	if s.vec == nil {
		return nil, nil
	}
	rows, err := s.read(ctx, "MATCH (v:`mem_entity`) WHERE v.`mem_entity`.`emb_model` != $model RETURN properties(v) AS p, v.`mem_entity`.`updated_at` AS at, v.`mem_entity`.`eid` AS eid"+
		" ORDER BY at, eid LIMIT $limit", map[string]any{"model": model, "limit": int64(store.ClampLimit(limit, 64))})
	if err != nil {
		return nil, err
	}
	var out []memory.Entity
	for _, r := range rows {
		p, _ := r["p"].(map[string]any)
		e, err := toEntity(p)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// SetMemoryEmbeddings implements store.MemoryStore: vectors go to the
// vector index with each entity's tenant and namespace, then the entities
// are marked as embedded by model.
func (s *Store) SetMemoryEmbeddings(ctx context.Context, model string, vecs map[string][]float32) error {
	if err := memory.CheckVectors(vecs); err != nil || len(vecs) == 0 || s.vec == nil {
		return err
	}
	ids := make([]string, 0, len(vecs))
	for id := range vecs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	rows, err := s.read(ctx, "FETCH PROP ON `mem_entity` "+literals(mapSlice(ids, entityVID))+
		" YIELD properties(vertex).eid AS eid, properties(vertex).tenant_id AS tenant, properties(vertex).ns AS ns", nil)
	if err != nil {
		return err
	}
	items := make([]vector.Item, 0, len(rows))
	for _, r := range rows {
		id, _ := r["eid"].(string)
		tenant, _ := r["tenant"].(string)
		namespace, _ := r["ns"].(string)
		items = append(items, vector.Item{ID: id, Tenant: tenant, Namespace: namespace, Vector: vecs[id]})
	}
	if err := s.vec.Upsert(ctx, model, items); err != nil {
		return fmt.Errorf("vector index: %w", err)
	}
	for _, it := range items {
		if _, err := s.write(ctx, "UPSERT VERTEX ON `mem_entity` "+literal(entityVID(it.ID))+" SET `emb_model` = $model", map[string]any{"model": model}); err != nil {
			return err
		}
	}
	return nil
}

// MemoryLexical implements store.MemoryStore over the tenant-rooted entity lookup.
func (s *Store) MemoryLexical(ctx context.Context, repo, query string, k int) ([]memory.Scored, error) {
	if len(memory.Words(query)) == 0 {
		return []memory.Scored{}, nil
	}
	entities, err := s.loadEntities(ctx, repo)
	if err != nil {
		return nil, err
	}
	return memory.RankBM25(entities, query, vector.ClampK(k)), nil
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
	entities, err := s.loadEntities(ctx, repo)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]memory.Entity, len(entities))
	for _, e := range entities {
		byID[e.ID] = e
	}
	for _, h := range hits {
		if e, ok := byID[h.ID]; ok {
			out = append(out, memory.Scored{Entity: e, Score: h.Score})
		}
	}
	return out[:min(len(out), k)], nil
}
