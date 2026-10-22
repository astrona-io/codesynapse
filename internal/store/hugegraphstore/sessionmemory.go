package hugegraphstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/sessionmemory"
	"github.com/parisnakitakejser/codesynapse/internal/vector"
)

const sessionRead = `def revoked=g.V().hasLabel('SessionMemoryRevocation').has('tenant_id',tenant).has('session_id',session).hasNext()
def scopes=g.V().hasLabel('SessionMemoryScope').has('tenant_id',tenant).has('session_id',session).has('ns',ns).values('doc').toList()
[[revoked:revoked,doc:scopes?scopes[0]:'']]`

func sessionBindings(scope sessionmemory.Scope) map[string]any {
	return map[string]any{"tenant": scope.TenantID, "session": scope.SessionID, "ns": scope.Namespace, "scopeVID": vid("ss", scope.Key())}
}

// LoadSessionDocument loads one tenant/session/namespace snapshot, checking revocation first.
func (s *Store) LoadSessionDocument(ctx context.Context, scope sessionmemory.Scope) (sessionmemory.Document, error) {
	if err := scope.Check(ctx); err != nil {
		return sessionmemory.Document{}, err
	}
	var rows []struct {
		Revoked bool   `json:"revoked"`
		Doc     string `json:"doc"`
	}
	if err := s.read(ctx, sessionRead, sessionBindings(scope), &rows); err != nil {
		return sessionmemory.Document{}, err
	}
	var d sessionmemory.Document
	if len(rows) > 0 {
		if rows[0].Revoked {
			return d, sessionmemory.ErrClosed
		}
		if rows[0].Doc != "" {
			if err := json.Unmarshal([]byte(rows[0].Doc), &d); err != nil {
				return d, err
			}
		}
	}
	return d, nil
}

const sessionCommit = `synchronized(graph) {
def revoked=g.V().hasLabel('SessionMemoryRevocation').has('tenant_id',tenant).has('session_id',session).hasNext()
if(revoked)return ['closed']
def roots=g.V().hasLabel('SessionMemoryScope').has('tenant_id',tenant).toList()
def root=roots.find{it.value('session_id')==session&&it.value('ns')==ns}
if((root?root.property('session_revision').orElse(0L):0L)!=expected)return ['conflict']
if(root&&(root.value('expires')!=expires||(!deleted&&(root.value('deleted')||root.value('expires')<=now))))return ['closed']
def retained=roots.findAll{it!=root}
if(retained.size()+1>maxScopes||retained.count{it.value('session_id')==session}+1>maxNamespaces)return ['limit']
def live=retained.findAll{!it.property('deleted').orElse(false)&&it.property('expires').orElse(0L)>now}
def local=live.findAll{it.value('session_id')==session}
if(!deleted&&(live.size()+1>maxScopes||local.size()+1>maxNamespaces||live.sum(0L){it.property('facts').orElse(0L)}+facts>maxTenantFacts||local.sum(0L){it.property('facts').orElse(0L)}+facts>maxSessionFacts))return ['limit']
def old=g.V().hasLabel('SessionMemoryEntity').has('tenant_id',tenant).has('session_id',session).has('ns',ns).toList()
old.each{it.remove()}
graph.addVertex(T.label,'SessionMemoryScope',T.id,scopeVID,'tenant_id',tenant,'session_id',session,'ns',ns,'doc',doc,'session_revision',revision,'expires',expires,'deleted',deleted,'facts',facts)
def vertices=[:]
entities.each{e->def v=graph.addVertex(T.label,'SessionMemoryEntity',T.id,e.v,'eid',e.id,'tenant_id',tenant,'session_id',session,'ns',ns,'name',e.name,'doc',e.doc,'expires',expires);vertices[e.id]=v}
factRows.each{f->vertices[f.subject].addEdge('SESSION_FACT',vertices[f.object],'fid',f.id,'tenant_id',tenant,'session_id',session,'ns',ns,'predicate',f.predicate,'doc',f.doc,'expires',expires)}
graph.tx().commit()
return ['ok']
}`

// CommitSessionDocument commits one revision and its materialized graph under the same scope.
func (s *Store) CommitSessionDocument(ctx context.Context, d sessionmemory.Document, expected int64) error {
	if err := sessionmemory.CheckCommit(d, expected); err != nil {
		return err
	}
	if err := d.Scope.Check(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	b := sessionBindings(d.Scope)
	entities := []map[string]any{}
	for _, e := range d.Graph.Entities {
		raw, _ := json.Marshal(e)
		entities = append(entities, map[string]any{"id": e.ID, "v": vid("se", e.ID), "name": e.Name, "doc": string(raw)})
	}
	facts := []map[string]any{}
	for _, f := range d.Graph.Facts {
		raw, _ := json.Marshal(f)
		facts = append(facts, map[string]any{"id": f.ID, "subject": f.Subject, "object": f.Object, "predicate": f.Predicate, "doc": string(raw)})
	}
	with(b, "doc", string(raw), "expected", expected, "revision", d.Revision, "expires", d.ExpiresAt.UnixMilli(), "deleted", d.Deleted, "facts", len(d.Graph.Facts), "now", time.Now().UnixMilli(), "entities", entities, "factRows", facts,
		"maxScopes", sessionmemory.MaxTenantScopes, "maxNamespaces", sessionmemory.MaxNamespaces, "maxTenantFacts", sessionmemory.MaxTenantFacts, "maxSessionFacts", sessionmemory.MaxSessionFacts)
	payload, err := s.gremlinBody(sessionCommit, b)
	if err != nil {
		return err
	}
	if len(payload) > s.maxRequest {
		return sessionmemory.ErrLimit
	}
	var result []string
	if err = s.write(ctx, sessionCommit, b, &result); err != nil {
		return err
	}
	if len(result) != 1 {
		return sessionmemory.ErrConflict
	}
	switch result[0] {
	case "closed":
		return sessionmemory.ErrClosed
	case "conflict":
		return sessionmemory.ErrConflict
	case "limit":
		return sessionmemory.ErrLimit
	case "ok":
	default:
		return sessionmemory.ErrConflict
	}
	if index, ok := s.vec.(vector.SessionIndex); ok {
		return index.DeleteSession(ctx, d.Scope)
	}
	return nil
}

const sessionRevoke = `synchronized(graph){
def scopes=g.V().hasLabel('SessionMemoryScope').has('tenant_id',tenant).has('session_id',session).toList()
def entities=g.V().hasLabel('SessionMemoryEntity').has('tenant_id',tenant).has('session_id',session).toList()
graph.addVertex(T.label,'SessionMemoryRevocation',T.id,scopeVID,'tenant_id',tenant,'session_id',session)
entities.each{it.remove()};scopes.each{it.property('deleted',true);it.property('doc','');it.property('facts',0L)}
graph.tx().commit();return [1]
}`

// RevokeSessionDocuments tombstones the execution and removes all of its scoped chat data.
func (s *Store) RevokeSessionDocuments(ctx context.Context, tenant, session string) error {
	if err := sessionmemory.CheckExecution(ctx, tenant, session); err != nil {
		return err
	}
	if err := s.write(ctx, sessionRevoke, map[string]any{"tenant": tenant, "session": session, "scopeVID": vid("sr", tenant+"|"+session)}, nil); err != nil {
		return err
	}
	if index, ok := s.vec.(vector.SessionIndex); ok {
		return index.DeleteExecution(ctx, tenant, session)
	}
	return nil
}

// WriteSessionVectors writes vectors only for the selected session namespace.
func (s *Store) WriteSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vecs map[string][]float32) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	index, ok := s.vec.(vector.SessionIndex)
	if !ok {
		return nil
	}
	d, err := s.LoadSessionDocument(ctx, scope)
	if err != nil {
		return err
	}
	if err = sessionmemory.CheckDocument(d, scope, time.Now()); err != nil {
		return err
	}
	items := []vector.Item{}
	for id, v := range vecs {
		items = append(items, vector.Item{ID: id, Tenant: scope.TenantID, SessionID: scope.SessionID, Namespace: scope.Namespace, ExpiresAt: d.ExpiresAt.UnixMilli(), Vector: v})
	}
	if err = index.UpsertSession(ctx, scope, model, items); err != nil {
		return err
	}
	latest, err := s.LoadSessionDocument(ctx, scope)
	if err == nil {
		err = sessionmemory.CheckDocument(latest, scope, time.Now())
	}
	if err == nil && latest.Revision != d.Revision {
		err = sessionmemory.ErrConflict
	}
	if err != nil {
		if clean := index.DeleteSession(ctx, scope); clean != nil {
			return clean
		}
		return err
	}
	return nil
}

// SearchSessionVectors searches only the selected session namespace.
func (s *Store) SearchSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vec []float32, k int) ([]memory.Scored, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	index, ok := s.vec.(vector.SessionIndex)
	if !ok {
		return []memory.Scored{}, nil
	}
	hits, err := index.SearchSession(ctx, scope, model, vec, k)
	if err != nil {
		return nil, err
	}
	out := []memory.Scored{}
	for _, h := range vector.IsolateSession(hits, scope, k) {
		out = append(out, memory.Scored{Entity: memory.Entity{ID: h.ID, Repo: scope.Key()}, Score: h.Score})
	}
	return out, nil
}
