package hugegraphstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

// Incremental publication (docs/PLAN.md, roadmap item 1). Publishing a
// repository used to rewrite every vertex and edge even when one file
// changed, and dominated the time of a sync (this repository: 3.4 s to
// extract, 22 s to publish). Every node vertex now carries content_hash (its
// properties) and edges_hash (its outgoing edges). A replacement reads those
// hashes under the repository's stage lease and stages only what differs:
// new and changed nodes with their outgoing edges, and the IDs of nodes that
// are gone. Changed vertices are updated in place, so edges from unchanged
// nodes into them survive. The result is the same graph a full replacement
// writes; when the stored state is not what was read, or most of the graph
// changed, the full replacement runs instead.

// deltaMaxShare is the share of changed nodes above which a full
// replacement is the cheaper write.
const deltaMaxShare = 0.5

// errStale means the stored graph changed between reading its hashes and
// committing the delta; the caller falls back to a full replacement.
var errStale = errors.New("stored graph changed since its hashes were read")

const readHashes = `g.V().hasLabel('node').has('repo',repo).project('v','h','e').by(id()).by(coalesce(values('content_hash'),constant(''))).by(coalesce(values('edges_hash'),constant(''))).toList()`

type storedHash struct {
	V string `json:"v"`
	H string `json:"h"`
	E string `json:"e"`
}

// hashRows sets content_hash and edges_hash on every node row and groups
// the edge rows by their source vertex.
func hashRows(nodeRows, edgeRows []map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, e := range edgeRows {
		a := e["a"].(string)
		out[a] = append(out[a], e)
	}
	for _, r := range nodeRows {
		v := r["v"].(string)
		p := r["p"].(map[string]any)
		// The hashes never cover themselves, so hashing again gives the same.
		delete(p, "content_hash")
		delete(p, "edges_hash")
		p["content_hash"] = digest(p)
		edges := out[v]
		slices.SortFunc(edges, func(x, y map[string]any) int {
			return strings.Compare(edgeKey(x), edgeKey(y))
		})
		p["edges_hash"] = digest(edges)
	}
	return out
}

func edgeKey(e map[string]any) string {
	b, _ := json.Marshal(e) //nolint:errcheck // plain maps of strings and integers
	return string(b)
}

// digest is a short hash of v's JSON (map keys are sorted, so it is stable).
func digest(v any) string {
	b, _ := json.Marshal(v) //nolint:errcheck // plain maps of strings, integers, booleans and lists
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// delta is what a replacement changes, or nil when a full replacement fits
// better (nothing stored yet, or most of the graph changed).
type delta struct {
	Nodes  []map[string]any  `json:"nodes"` // n: a new vertex; x: its edges are rewritten
	Edges  []map[string]any  `json:"edges"`
	Gone   []string          `json:"gone"`
	Expect map[string]string `json:"expect"` // vertex -> "content|edges" read before
	Total  int               `json:"total"`
}

func planDelta(stored []storedHash, nodeRows []map[string]any, bySource map[string][]map[string]any) *delta {
	if len(stored) == 0 {
		return nil
	}
	have := make(map[string]storedHash, len(stored))
	for _, s := range stored {
		have[s.V] = s
	}
	d := &delta{Nodes: []map[string]any{}, Edges: []map[string]any{}, Gone: []string{}, Expect: map[string]string{}, Total: len(stored)}
	seen := make(map[string]bool, len(nodeRows))
	for _, r := range nodeRows {
		v := r["v"].(string)
		seen[v] = true
		p := r["p"].(map[string]any)
		old, ok := have[v]
		if ok && old.H == p["content_hash"] && old.E == p["edges_hash"] {
			continue
		}
		row := map[string]any{"v": v, "p": p}
		if ok {
			d.Expect[v] = old.H + "|" + old.E
		} else {
			row["n"] = true
		}
		// Unchanged outgoing edges stay as they are; a node whose properties
		// changed (the Repository node does on every ingest) keeps them.
		if !ok || old.E != p["edges_hash"] {
			row["x"] = true
			d.Edges = append(d.Edges, bySource[v]...)
		}
		d.Nodes = append(d.Nodes, row)
	}
	for _, s := range stored {
		if !seen[s.V] {
			d.Gone = append(d.Gone, s.V)
		}
	}
	if float64(len(d.Nodes)+len(d.Gone)) > deltaMaxShare*float64(max(len(nodeRows), 1)) {
		return nil
	}
	return d
}

func (s *Store) storedHashes(ctx context.Context, repo string) ([]storedHash, error) {
	var out []storedHash
	err := s.read(ctx, readHashes, map[string]any{"repo": repo}, &out)
	return out, err
}

// commitDelta applies a staged delta in one transaction. Every vertex it
// touches is read and checked before the first write (HugeGraph refuses
// indexed reads after writes).
const commitDelta = `synchronized(graph){
def head=g.V(headID).tryNext()
if(!head.isPresent()||head.get().value('stage_run')!=run||head.get().value('stage_expires')<=now)return ['conflict']
def chunks=g.V().hasLabel('ingest_stage').has('repo',repo).limit(maxChunks+1).toList().sort{it.value('stage_index')}
if(chunks.size()!=chunkCount||chunks.any{it.value('stage_run')!=run})return ['incomplete']
def buffer=new java.io.ByteArrayOutputStream()
for(int i=0;i<chunks.size();i++){
 if(chunks[i].value('stage_index')!=i)return ['incomplete']
 def bytes=java.util.Base64.getDecoder().decode(chunks[i].value('stage_data'))
 if(buffer.size()+bytes.length>maxBytes)return ['limit']
 buffer.write(bytes)
}
def doc=new groovy.json.JsonSlurperClassic().parseText(buffer.toString('UTF-8'))
if(g.V().hasLabel('node').has('repo',repo).count().next()!=doc.total)return ['stale']
def changedIDs=doc.nodes.findAll{!it.n}.collect{it.v}
def byId=[:]
if(changedIDs){g.V(changedIDs as Object[]).toList().each{byId[it.id().asString()]=it}}
if(byId.size()!=changedIDs.size())return ['stale']
for(id in changedIDs){
 def v=byId[id]
 def h=v.property('content_hash').isPresent()?v.value('content_hash'):''
 def e=v.property('edges_hash').isPresent()?v.value('edges_hash'):''
 if(v.value('repo')!=repo||h+'|'+e!=doc.expect[id])return ['stale']
}
def goneV=doc.gone?g.V(doc.gone as Object[]).toList():[]
if(goneV.size()!=doc.gone.size()||goneV.any{it.value('repo')!=repo})return ['stale']
def newIDs=doc.nodes.findAll{it.n}.collect{it.v} as Set
def targetIDs=doc.edges.collect{it.b}.findAll{!byId.containsKey(it)&&!newIDs.contains(it)}.unique()
def targets=[:]
if(targetIDs){g.V(targetIDs as Object[]).toList().each{targets[it.id().asString()]=it}}
if(targets.size()!=targetIDs.size())return ['stale']
def rewired=doc.nodes.findAll{it.x&&!it.n}.collect{byId[it.v]}
def oldEdges=rewired.collectMany{it.edges(Direction.OUT).toList()}
def edgeOps=oldEdges.size()+doc.edges.size()
def vertexOps=goneV.size()+doc.nodes.size()+chunks.size()+1
if(edgeOps>txCapacity||vertexOps>txCapacity)return ['capacity',edgeOps.toString(),vertexOps.toString()]
try{
oldEdges.each{it.remove()}
goneV.each{it.remove()}
def vertices=[:]
doc.nodes.each{r->
 if(r.n){
  def kv=[T.label,'node',T.id,r.v,'run',run]
  r.p.each{k,val->kv<<k;kv<<val}
  vertices[r.v]=graph.addVertex(kv as Object[])
 }else{
  def v=byId[r.v]
  // Clear, then set: assigning a list property may add to it instead of replacing it.
  v.properties().toList().each{if(!['nid','repo','kind'].contains(it.key()))it.remove()}
  r.p.each{k,val->if(!['nid','repo','kind'].contains(k))v.property(k,val)}
  v.property('run',run)
  vertices[r.v]=v
 }
}
doc.edges.each{r->def kv=['count',r.c as Long,'run',run];if(r.e){kv<<'evidence';kv<<r.e};vertices[r.a].addEdge(r.l,vertices[r.b]?:targets[r.b],kv as Object[])}
chunks.each{it.remove()}
head.get().property('stage_run','');head.get().property('stage_expires',0L)
graph.tx().commit()
}catch(Throwable t){
graph.tx().rollback();throw t
}
return ['ok',doc.nodes.size().toString(),doc.gone.size().toString()]
}`
