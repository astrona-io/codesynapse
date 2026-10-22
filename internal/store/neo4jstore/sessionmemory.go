package neo4jstore

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

func sessionParams(scope sessionmemory.Scope) map[string]any {
	return map[string]any{"tenant": scope.TenantID, "session": scope.SessionID, "ns": scope.Namespace, "key": scope.Key()}
}

const sessionLoad = `OPTIONAL MATCH (r:SessionMemoryRevocation {tenant_id:$tenant,session_id:$session})
OPTIONAL MATCH (s:SessionMemoryScope {tenant_id:$tenant,session_id:$session,ns:$ns})
RETURN r IS NOT NULL AS revoked,s.doc AS doc`

// LoadSessionDocument loads one tenant/session/namespace snapshot, checking revocation first.
func (s *Store) LoadSessionDocument(ctx context.Context, scope sessionmemory.Scope) (sessionmemory.Document, error) {
	if err := scope.Check(ctx); err != nil {
		return sessionmemory.Document{}, err
	}
	r, err := s.read(ctx, sessionLoad, sessionParams(scope))
	if err != nil {
		return sessionmemory.Document{}, err
	}
	var d sessionmemory.Document
	if len(r.Records) > 0 {
		if r.Records[0].AsMap()["revoked"] == true {
			return d, sessionmemory.ErrClosed
		}
		if raw := str(r.Records[0], "doc"); raw != "" {
			err = json.Unmarshal([]byte(raw), &d)
		}
	}
	return d, err
}

// CommitSessionDocument commits one revision and its materialized graph under the same scope.
func (s *Store) CommitSessionDocument(ctx context.Context, d sessionmemory.Document, expected int64) error {
	if err := sessionmemory.CheckCommit(d, expected); err != nil {
		return err
	}
	if err := d.Scope.Check(ctx); err != nil {
		return err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	p := sessionParams(d.Scope)
	p["doc"], p["expected"], p["revision"], p["expires"], p["deleted"], p["facts"] = string(data), expected, d.Revision, d.ExpiresAt.UnixMilli(), d.Deleted, len(d.Graph.Facts)
	entities := []map[string]any{}
	for _, e := range d.Graph.Entities {
		raw, _ := json.Marshal(e)
		entities = append(entities, map[string]any{"id": e.ID, "name": e.Name, "doc": string(raw)})
	}
	facts := []map[string]any{}
	for _, f := range d.Graph.Facts {
		raw, _ := json.Marshal(f)
		facts = append(facts, map[string]any{"id": f.ID, "subject": f.Subject, "object": f.Object, "predicate": f.Predicate, "doc": string(raw)})
	}
	p["entities"], p["rows"] = entities, facts
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.db, AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx) //nolint:errcheck // transaction errors take precedence
	_, err = sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		run := func(q string) ([]*neo4j.Record, error) {
			r, e := tx.Run(ctx, q, p)
			if e != nil {
				return nil, e
			}
			return r.Collect(ctx)
		}
		if _, err := run(`MERGE (t:SessionMemoryTenant {tenant_id:$tenant}) SET t.revision=coalesce(t.revision,0)+1`); err != nil {
			return nil, err
		}
		r, err := run(sessionLoad)
		if err != nil {
			return nil, err
		}
		if len(r) > 0 {
			if r[0].AsMap()["revoked"] == true {
				return nil, sessionmemory.ErrClosed
			}
			var old sessionmemory.Document
			if raw := str(r[0], "doc"); raw != "" {
				if err = json.Unmarshal([]byte(raw), &old); err != nil {
					return nil, err
				}
			}
			if old.Revision != expected {
				return nil, sessionmemory.ErrConflict
			}
			if err = sessionmemory.CheckTransition(old, d, time.Now()); err != nil {
				return nil, err
			}
		}
		r, err = run(`MATCH (s:SessionMemoryScope {tenant_id:$tenant}) RETURN s.session_id AS session,s.ns AS ns,s.facts AS facts,s.expires AS expires,s.deleted AS deleted`)
		if err != nil {
			return nil, err
		}
		quota := []sessionmemory.Quota{}
		for _, rec := range r {
			m := rec.AsMap()
			count, _ := m["facts"].(int64)
			expires, _ := m["expires"].(int64)
			deleted, _ := m["deleted"].(bool)
			quota = append(quota, sessionmemory.Quota{Scope: sessionmemory.Scope{TenantID: d.Scope.TenantID, SessionID: str(rec, "session"), Namespace: str(rec, "ns")}, Facts: int(count), ExpiresAt: time.UnixMilli(expires), Deleted: deleted})
		}
		if err = sessionmemory.CheckQuota(quota, d, time.Now()); err != nil {
			return nil, err
		}
		for _, q := range []string{
			`MATCH (e:SessionMemoryEntity {tenant_id:$tenant,session_id:$session,ns:$ns}) DETACH DELETE e`,
			`MERGE (s:SessionMemoryScope {tenant_id:$tenant,session_id:$session,ns:$ns}) SET s.doc=$doc,s.revision=$revision,s.expires=$expires,s.deleted=$deleted,s.facts=$facts`,
			`UNWIND $entities AS row CREATE (e:SessionMemoryEntity {id:row.id,tenant_id:$tenant,session_id:$session,ns:$ns,name:row.name,doc:row.doc,expires:$expires})`,
			`UNWIND $rows AS row MATCH (a:SessionMemoryEntity {id:row.subject,tenant_id:$tenant,session_id:$session,ns:$ns}),(b:SessionMemoryEntity {id:row.object,tenant_id:$tenant,session_id:$session,ns:$ns}) CREATE (a)-[:SESSION_FACT {id:row.id,tenant_id:$tenant,session_id:$session,ns:$ns,predicate:row.predicate,doc:row.doc,expires:$expires}]->(b)`,
		} {
			if _, err = run(q); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

// RevokeSessionDocuments tombstones the execution and removes all of its scoped chat data.
func (s *Store) RevokeSessionDocuments(ctx context.Context, tenant, session string) error {
	if err := sessionmemory.CheckExecution(ctx, tenant, session); err != nil {
		return err
	}
	p := map[string]any{"tenant": tenant, "session": session}
	_, err := s.write(ctx, `MERGE (t:SessionMemoryTenant {tenant_id:$tenant}) SET t.revision=coalesce(t.revision,0)+1
MERGE (:SessionMemoryRevocation {tenant_id:$tenant,session_id:$session})
WITH t OPTIONAL MATCH (e:SessionMemoryEntity {tenant_id:$tenant,session_id:$session}) DETACH DELETE e`, p)
	if err != nil {
		return err
	}
	_, err = s.write(ctx, `MATCH (s:SessionMemoryScope {tenant_id:$tenant,session_id:$session}) SET s.deleted=true,s.doc='',s.facts=0`, p)
	return err
}

// WriteSessionVectors writes vectors only for the selected session namespace.
func (s *Store) WriteSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vecs map[string][]float32) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	p := sessionParams(scope)
	p["model"] = model
	rows := []map[string]any{}
	for id, v := range vecs {
		rows = append(rows, map[string]any{"id": id, "vector": v})
	}
	p["rows"] = rows
	_, err := s.write(ctx, `MATCH (s:SessionMemoryScope {tenant_id:$tenant,session_id:$session,ns:$ns}) WHERE NOT s.deleted AND s.expires>timestamp()
AND NOT EXISTS { MATCH (:SessionMemoryRevocation {tenant_id:$tenant,session_id:$session}) }
UNWIND $rows AS row MATCH (e:SessionMemoryEntity {id:row.id,tenant_id:$tenant,session_id:$session,ns:$ns}) SET e.emb_model=$model,e.embedding=row.vector`, p)
	return err
}

// SearchSessionVectors searches only the selected session namespace.
func (s *Store) SearchSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vec []float32, k int) ([]memory.Scored, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	p := sessionParams(scope)
	p["model"] = model
	p["limit"] = sessionmemory.MaxEntities
	r, err := s.read(ctx, `MATCH (e:SessionMemoryEntity {tenant_id:$tenant,session_id:$session,ns:$ns,emb_model:$model}) WHERE e.expires>timestamp() RETURN e.id AS id,e.embedding AS embedding LIMIT $limit`, p)
	if err != nil {
		return nil, err
	}
	hits := []memory.Scored{}
	for _, rec := range r.Records {
		raw, _ := rec.AsMap()["embedding"].([]any)
		v := make([]float32, len(raw))
		for i, x := range raw {
			f, _ := x.(float64)
			v[i] = float32(f)
		}
		if len(v) == len(vec) {
			hits = append(hits, memory.Scored{Entity: memory.Entity{ID: str(rec, "id"), Repo: scope.Key()}, Score: memory.Similarity(v, vec)})
		}
	}
	return rankSession(hits, k), nil
}

func rankSession(hits []memory.Scored, k int) []memory.Scored {
	slices.SortFunc(hits, func(a, b memory.Scored) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
	return hits[:min(len(hits), k)]
}
