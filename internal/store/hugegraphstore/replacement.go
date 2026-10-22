package hugegraphstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

const stageChunkBytes = 24 << 10
const maxStageChunks = (store.MaxReplacementBytes + stageChunkBytes - 1) / stageChunkBytes

const stageSchema = `def s=graph.schema()
['stage_data','stage_run'].each{s.propertyKey(it).asText().ifNotExist().create()}
['stage_index','stage_expires'].each{s.propertyKey(it).asLong().ifNotExist().create()}
s.vertexLabel('ingest_stage').useCustomizeStringId().properties('repo','stage_run','stage_index','stage_data','stage_expires').ifNotExist().create()
s.vertexLabel('ingest_head').useCustomizeStringId().properties('repo','stage_run','stage_expires').ifNotExist().create()
s.indexLabel('ingestStageByRepo').onV('ingest_stage').by('repo').secondary().ifNotExist().create()
s.indexLabel('ingestStageByExpiry').onV('ingest_stage').by('stage_expires').range().ifNotExist().create()
1`

const pruneStages = `def stale=g.V().hasLabel('ingest_stage').has('stage_expires',lt(now)).limit(128).toList()
stale.each{it.remove()}
stale.size()`

const acquireStage = `synchronized(graph){
def head=g.V(headID).tryNext()
if(head.isPresent()&&head.get().value('stage_run')!=''&&head.get().value('stage_expires')>now)return ['busy']
def old=g.V().hasLabel('ingest_stage').has('repo',repo).limit(maxChunks+1).toList()
if(old.size()>maxChunks)return ['limit']
old.each{it.remove()}
graph.addVertex(T.label,'ingest_head',T.id,headID,'repo',repo,'stage_run',run,'stage_expires',expires)
graph.tx().commit();return ['ok']
}`

const uploadStage = `synchronized(graph){
def head=g.V(headID).tryNext()
if(!head.isPresent()||head.get().value('stage_run')!=run||head.get().value('stage_expires')<=now)return ['conflict']
graph.addVertex(T.label,'ingest_stage',T.id,chunkID,'repo',repo,'stage_run',run,'stage_index',idx,'stage_data',data,'stage_expires',expires)
graph.tx().commit();return ['ok']
}`

const abandonStage = `synchronized(graph){
def head=g.V(headID).tryNext()
if(!head.isPresent()||head.get().value('stage_run')!=run)return ['conflict']
def chunks=g.V().hasLabel('ingest_stage').has('repo',repo).limit(maxChunks+1).toList()
if(chunks.size()>maxChunks)return ['limit']
chunks.each{it.remove()};head.get().property('stage_run','');head.get().property('stage_expires',0L)
graph.tx().commit();return ['ok']
}`

const commitStage = `synchronized(graph){
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
def old=g.V().hasLabel('node').has('repo',repo).toList()
def oldEdges=old.collectMany{it.edges(Direction.OUT).toList()}
def ids=doc.nodes.collect{it.v} as Set
// Read every index before modifying the graph; HugeGraph refuses indexed reads after writes.
oldEdges.each{it.remove()}
old.findAll{!ids.contains(it.id().asString())}.each{it.remove()}
def vertices=[:]
doc.nodes.each{r->
 def kv=[T.label,'node',T.id,r.v,'run',run]
 r.p.each{k,val->kv<<k;kv<<val}
 vertices[r.v]=graph.addVertex(kv as Object[])
}
doc.edges.each{r->vertices[r.a].addEdge(r.l,vertices[r.b],'count',r.c as Long,'run',run)}
chunks.each{it.remove()}
head.get().property('stage_run','');head.get().property('stage_expires',0L)
graph.tx().commit();return ['ok']
}`

func (s *Store) replaceAtomic(ctx context.Context, repo string, nodes []model.Node, edges []model.Edge) error {
	run, err := newRunID()
	if err != nil {
		return fmt.Errorf("generate ingest run: %w", err)
	}
	nodeRows := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		p := nodeprops.To(n)
		p["nid"] = p["id"]
		delete(p, "id")
		nodeRows = append(nodeRows, map[string]any{"v": nodeVID(n.ID), "p": p})
	}
	edgeRows := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		edgeRows = append(edgeRows, map[string]any{"a": nodeVID(e.From), "b": nodeVID(e.To), "l": string(e.Kind), "c": int64(max(e.Count, 1))})
	}
	payload, err := json.Marshal(map[string]any{"nodes": nodeRows, "edges": edgeRows})
	if err != nil {
		return err
	}
	if len(payload) > store.MaxReplacementBytes {
		return errors.New("repository replacement exceeds 64 MiB")
	}
	chunks := (len(payload) + stageChunkBytes - 1) / stageChunkBytes
	now := time.Now()
	bindings := map[string]any{"repo": repo, "headID": vid("ih", repo), "run": run, "now": now.UnixMilli(), "expires": now.Add(2 * time.Hour).UnixMilli(), "maxChunks": maxStageChunks}
	if err := s.write(ctx, pruneStages, map[string]any{"now": now.UnixMilli()}, nil); err != nil {
		return fmt.Errorf("prune abandoned ingest stages: %w", err)
	}
	if err := s.stageWrite(ctx, acquireStage, bindings); err != nil {
		return fmt.Errorf("reserve ingest stage: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		// Cleanup keeps the original identity but survives request cancellation.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.stageWrite(cleanup, abandonStage, bindings)
	}()
	for index, start := 0, 0; start < len(payload); index, start = index+1, start+stageChunkBytes {
		b := map[string]any{}
		for key, value := range bindings {
			b[key] = value
		}
		b["idx"] = index
		b["chunkID"] = vid("is", run+"|"+strconv.Itoa(index))
		b["data"] = base64.StdEncoding.EncodeToString(payload[start:min(start+stageChunkBytes, len(payload))])
		b["now"] = time.Now().UnixMilli()
		if err := s.stageWrite(ctx, uploadStage, b); err != nil {
			return fmt.Errorf("upload ingest stage: %w", err)
		}
	}
	bindings["chunkCount"] = chunks
	bindings["maxBytes"] = store.MaxReplacementBytes
	bindings["now"] = time.Now().UnixMilli()
	if err := store.CommitCheck(ctx); err != nil {
		return err
	}
	if err := s.stageWrite(ctx, commitStage, bindings); err != nil {
		return fmt.Errorf("commit ingest stage: %w", err)
	}
	committed = true
	return nil
}

func (s *Store) stageWrite(ctx context.Context, script string, bindings map[string]any) error {
	var result []string
	if err := s.write(ctx, script, bindings, &result); err != nil {
		return err
	}
	if len(result) != 1 || result[0] != "ok" {
		return errors.New("ingest stage rejected; retry after the active replacement completes")
	}
	return nil
}
