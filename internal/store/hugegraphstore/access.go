package hugegraphstore

import (
	"context"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

const accessSchema = `def s=graph.schema()
['aid','user_id','team_id','repo_id','graph_id','default_repo_access','role','digest'].each { s.propertyKey(it).asText().ifNotExist().create() }
['policy_revision','rank'].each { s.propertyKey(it).asLong().ifNotExist().create() }
['active','inherits_default','usable'].each { s.propertyKey(it).asBoolean().ifNotExist().create() }
def labels = [Organization:['aid','tenant_id','policy_revision','default_repo_access'],
User:['aid','tenant_id','policy_revision','user_id','active','inherits_default'],
Team:['aid','tenant_id','policy_revision','team_id'],
AccessRepository:['aid','tenant_id','policy_revision','repo_id','graph_id','usable'],
AccessHead:['aid','tenant_id','policy_revision','digest']]
def existingLabels=s.getVertexLabels().collect{it.name()}
labels.each { name,props ->
 if(!existingLabels.contains(name)){s.vertexLabel(name).useCustomizeStringId().properties(props as String[]).create()}
 def have=s.getVertexLabel(name).properties().collect{graph.propertyKey(it).name()}
 def missing=props-have
 if(missing){s.vertexLabel(name).properties(missing as String[]).nullableKeys(missing as String[]).append()}
 s.indexLabel(name+'ByTenant').onV(name).by('tenant_id').secondary().ifNotExist().create()
}
def edges=[USER_MEMBER_OF_ORG:['User','Organization'],USER_MEMBER_OF_TEAM:['User','Team'],TEAM_BELONGS_TO_ORG:['Team','Organization'],REPO_BELONGS_TO_ORG:['AccessRepository','Organization'],USER_HAS_ACCESS:['User','AccessRepository'],TEAM_HAS_ACCESS:['Team','AccessRepository']]
edges.each { name,ends -> s.edgeLabel(name).sourceLabel(ends[0]).targetLabel(ends[1]).properties('tenant_id','role','rank').ifNotExist().create() }
1`

const writeAccessVertices = `rows.each { p ->
 def kv=[T.id,p.aid,T.label,vlabel];p.each { k,v -> kv<<k;kv<<v };graph.addVertex(kv as Object[])
};rows.size()`
const writeAccessEdges = `def pairs=rows.collect { p -> [p:p,a:g.V(p.from).next(),b:g.V(p.to).next()] }
pairs.each { x -> x.a.addEdge(x.p.physical,x.b,'tenant_id',x.p.tenant_id,'role',x.p.role,'rank',x.p.rank) };rows.size()`
const publishAccess = `graph.addVertex(T.id,head,T.label,'AccessHead','aid',head,'tenant_id',tenant,'policy_revision',revision,'digest',digest);1`
const accessHeads = `def hs=g.V().hasLabel('AccessHead').has('tenant_id',tenant).order().by('policy_revision',Order.desc).limit(2).toList()
hs.collect{[revision:it.value('policy_revision'),digest:it.value('digest')]}`
const pruneAccess = `def hs=g.V().hasLabel('AccessHead').has('tenant_id',tenant).order().by('policy_revision',Order.desc).limit(1).toList()
if(hs.isEmpty()) return 0
def current=hs[0].value('policy_revision')
def vs=g.V().hasLabel(vlabel).has('tenant_id',tenant).filter{it.get().value('policy_revision')<current}.limit(batch).toList()
vs.each{it.remove()};vs.size()`

// ReplaceAccess publishes a revision only after every vertex and edge is stored.
func (s *Store) ReplaceAccess(ctx context.Context, snapshot model.AccessSnapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	digest := snapshot.Digest()
	current, published, err := s.accessHead(ctx, snapshot.TenantID)
	if err != nil {
		return err
	}
	if current > snapshot.Revision {
		return nil
	}
	if current == snapshot.Revision {
		if published != digest {
			return fmt.Errorf("access revision already has a different policy")
		}
		return nil
	}
	nodes, edges := store.AccessRows(snapshot)
	for kind, rows := range nodes {
		if err := s.batched(ctx, writeAccessVertices, rows, map[string]any{"vlabel": string(kind)}); err != nil {
			return err
		}
	}
	if err := s.batched(ctx, writeAccessEdges, edges, nil); err != nil {
		return err
	}
	b := map[string]any{"tenant": snapshot.TenantID, "revision": snapshot.Revision, "digest": digest, "head": model.AccessVertexID(snapshot.TenantID, snapshot.Revision, digest, "AccessHead", snapshot.TenantID), "batch": deleteBatch}
	if err := s.write(ctx, publishAccess, b, nil); err != nil {
		return err
	}
	if current, published, err := s.accessHead(ctx, snapshot.TenantID); err != nil {
		return err
	} else if current == snapshot.Revision && published != digest {
		return fmt.Errorf("conflicting published access policies")
	}
	for _, kind := range []model.NodeKind{model.KindOrganization, model.KindUser, model.KindTeam, model.KindAccessRepository, "AccessHead"} {
		b["vlabel"] = string(kind)
		if err := s.drain(ctx, pruneAccess, b); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) accessHead(ctx context.Context, tenant string) (int64, string, error) {
	var heads []struct {
		Revision int64  `json:"revision"`
		Digest   string `json:"digest"`
	}
	if err := s.read(ctx, accessHeads, map[string]any{"tenant": tenant}, &heads); err != nil {
		return 0, "", err
	}
	if len(heads) == 0 {
		return 0, "", nil
	}
	if len(heads) > 1 && heads[0].Revision == heads[1].Revision {
		return heads[0].Revision, "", nil
	}
	return heads[0].Revision, heads[0].Digest, nil
}

const accessQuery = `def hs=g.V().hasLabel('AccessHead').has('tenant_id',tenant).order().by('policy_revision',Order.desc).limit(2).toList()
if(hs.isEmpty() || hs[0].value('policy_revision')!=revision || hs[0].value('digest')!=digest || (hs.size()>1 && hs[1].value('policy_revision')==revision)) return []
def os=g.V().hasLabel('Organization').has('tenant_id',tenant).hasId(org).limit(2).toList()
if(os.size()!=1) return []
def us=g.V().hasLabel('User').has('tenant_id',tenant).hasId(user).has('active',true)
 .where(__.outE('USER_MEMBER_OF_ORG').has('tenant_id',tenant).inV().hasId(org)).limit(2).toList()
def rs=g.V().hasLabel('AccessRepository').has('tenant_id',tenant).hasId(repo).has('usable',true)
 .where(__.outE('REPO_BELONGS_TO_ORG').has('tenant_id',tenant).inV().hasId(org)).limit(2).toList()
if(us.size()!=1 || rs.size()!=1) return []
def u=us[0];def r=rs[0]
def valid={e,allowNone ->
 if(!e.property('role').isPresent() || !e.property('rank').isPresent()) return false
 def rank=['NONE':0,'READ':1,'WRITE':2,'ADMIN':3][e.value('role')]
 rank!=null && (allowNone || rank>0) && e.value('rank')==rank
}
def direct=g.V(u.id()).outE('USER_HAS_ACCESS').has('tenant_id',tenant).where(__.inV().hasId(r.id())).limit(2).toList()
if(direct.size()>1 || direct.any{!valid(it,true)}) return []
def teams=g.V(u.id()).outE('USER_MEMBER_OF_TEAM').has('tenant_id',tenant).inV().hasLabel('Team').has('tenant_id',tenant)
 .where(__.outE('TEAM_BELONGS_TO_ORG').has('tenant_id',tenant).inV().hasId(org)).dedup().limit(probe).toList()
if(teams.size()>=probe) return []
def ranks=[]
for(t in teams){
 def grants=g.V(t.id()).outE('TEAM_HAS_ACCESS').has('tenant_id',tenant).where(__.inV().hasId(r.id())).limit(2).toList()
 if(grants.size()>1 || grants.any{!valid(it,false)}) return []
 ranks.addAll(grants.collect{it.value('rank')})
}
def fallback=u.value('inherits_default') && os[0].value('default_repo_access')=='READ_ONLY' ? 1 : 0
def rank=direct.size()==1 ? direct[0].value('rank') : (ranks.isEmpty() ? fallback : ranks.max())
[[graph_id:r.value('graph_id'),rank:rank]]`

// AccessAt implements store.AccessStore using a constant, tenant-rooted traversal.
func (s *Store) AccessAt(ctx context.Context, tenant string, revision int64, userID, repoID string) (model.AccessDecision, error) {
	if tenant == "" || revision < 1 || userID == "" || repoID == "" {
		return model.AccessDecision{}, nil
	}
	current, digest, err := s.accessHead(ctx, tenant)
	if err != nil {
		return model.AccessDecision{}, err
	}
	if current != revision || digest == "" {
		return model.AccessDecision{}, nil
	}
	b := map[string]any{"tenant": tenant, "revision": revision, "digest": digest, "org": model.AccessVertexID(tenant, revision, digest, model.KindOrganization, tenant),
		"user": model.AccessVertexID(tenant, revision, digest, model.KindUser, userID), "repo": model.AccessVertexID(tenant, revision, digest, model.KindAccessRepository, repoID), "probe": model.MaxAccessTeams + 1}
	var rows []struct {
		GraphID string `json:"graph_id"`
		Rank    int    `json:"rank"`
	}
	if err := s.read(ctx, accessQuery, b, &rows); err != nil {
		return model.AccessDecision{}, err
	}
	if len(rows) != 1 {
		return model.AccessDecision{}, nil
	}
	return model.AccessDecision{GraphID: rows[0].GraphID, Rank: rows[0].Rank}, nil
}
