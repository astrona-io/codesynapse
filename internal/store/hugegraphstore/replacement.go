package hugegraphstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

const stageChunkBytes = 24 << 10
const maxStageChunks = (store.MaxReplacementBytes + stageChunkBytes - 1) / stageChunkBytes

const stageSchema = `def s=graph.schema()
['stage_data','stage_run','pub_gen'].each{s.propertyKey(it).asText().ifNotExist().create()}
['stage_index','stage_expires'].each{s.propertyKey(it).asLong().ifNotExist().create()}
s.vertexLabel('ingest_stage').useCustomizeStringId().properties('repo','stage_run','stage_index','stage_data','stage_expires').ifNotExist().create()
s.vertexLabel('ingest_head').useCustomizeStringId().properties('repo','stage_run','stage_expires','pub_gen').nullableKeys('pub_gen').ifNotExist().create()
if(!s.getVertexLabel('ingest_head').properties().collect{graph.propertyKey(it).name()}.contains('pub_gen')){s.vertexLabel('ingest_head').properties('pub_gen').nullableKeys('pub_gen').append()}
s.indexLabel('ingestStageByRepo').onV('ingest_stage').by('repo').secondary().ifNotExist().create()
s.indexLabel('ingestStageByExpiry').onV('ingest_stage').by('stage_expires').range().ifNotExist().create()
1`

const pruneStages = `def stale=g.V().hasLabel('ingest_stage').has('stage_expires',lt(now)).limit(128).toList()
stale.each{it.remove()}
stale.size()`

// acquireStage takes the lease and answers the publication generation:
// the run that last committed this repository's graph ("" when unknown).
const acquireStage = `synchronized(graph){
def head=g.V(headID).tryNext()
if(head.isPresent()&&head.get().value('stage_run')!=''&&head.get().value('stage_expires')>now)return ['busy']
def gen=head.isPresent()&&head.get().property('pub_gen').isPresent()?head.get().value('pub_gen'):''
def old=g.V().hasLabel('ingest_stage').has('repo',repo).limit(maxChunks+1).toList()
if(old.size()>maxChunks)return ['limit']
old.each{it.remove()}
graph.addVertex(T.label,'ingest_head',T.id,headID,'repo',repo,'stage_run',run,'stage_expires',expires,'pub_gen',gen)
graph.tx().commit();return ['ok',gen]
}`

// clearGeneration forgets the publication generation before a repository's
// nodes are deleted, so no cached hashes are trusted afterwards.
const clearGeneration = `def head=g.V(headID).tryNext()
if(head.isPresent()){head.get().property('pub_gen','');graph.tx().commit()}
1`

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
def gone=old.findAll{!ids.contains(it.id().asString())}
// The whole replacement is one transaction; refuse before writing when it
// cannot fit the server's vertex.tx_capacity or edge.tx_capacity.
def edgeOps=oldEdges.size()+doc.edges.size()
def vertexOps=gone.size()+doc.nodes.size()+chunks.size()+1
if(edgeOps>txCapacity||vertexOps>txCapacity)return ['capacity',edgeOps.toString(),vertexOps.toString()]
try{
// Read every index before modifying the graph; HugeGraph refuses indexed reads after writes.
oldEdges.each{it.remove()}
gone.each{it.remove()}
def vertices=[:]
doc.nodes.each{r->
 def kv=[T.label,'node',T.id,r.v,'run',run]
 r.p.each{k,val->kv<<k;kv<<val}
 vertices[r.v]=graph.addVertex(kv as Object[])
}
doc.edges.each{r->def kv=['count',r.c as Long,'run',run];if(r.e){kv<<'evidence';kv<<r.e};vertices[r.a].addEdge(r.l,vertices[r.b],kv as Object[])}
chunks.each{it.remove()}
head.get().property('stage_run','');head.get().property('stage_expires',0L);head.get().property('pub_gen',run)
graph.tx().commit()
}catch(Throwable t){
// Never leave a half-applied replacement for the next request on this thread to commit.
graph.tx().rollback();throw t
}
return ['ok']
}`

func (s *Store) replaceAtomic(ctx context.Context, repo string, nodes []model.Node, edges []model.Edge) error {
	nodeRows := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		p := nodeprops.To(n)
		p["nid"] = p["id"]
		delete(p, "id")
		nodeRows = append(nodeRows, map[string]any{"v": nodeVID(n.ID), "p": p})
	}
	edgeRows := make([]map[string]any, 0, len(edges))
	for _, e := range edges {
		row := map[string]any{"a": nodeVID(e.From), "b": nodeVID(e.To), "l": string(e.Kind), "c": int64(max(e.Count, 1))}
		if e.Evidence != "" {
			row["e"] = string(e.Evidence)
		}
		edgeRows = append(edgeRows, row)
	}
	bySource := hashRows(nodeRows, edgeRows)
	payload, err := json.Marshal(map[string]any{"nodes": nodeRows, "edges": edgeRows})
	if err != nil {
		return err
	}
	if len(payload) > store.MaxReplacementBytes {
		return errors.New("repository replacement exceeds 64 MiB")
	}
	if err := s.write(ctx, pruneStages, map[string]any{"now": time.Now().UnixMilli()}, nil); err != nil {
		return fmt.Errorf("prune abandoned ingest stages: %w", err)
	}
	// Under the stage lease nobody else publishes this repository, so the
	// hashes read here describe what the delta is applied to.
	published := hashesOf(nodeRows)
	err = s.underStage(ctx, repo, func(bindings map[string]any) (bool, error) {
		// The hashes this process published last are still what is stored
		// when nobody committed since (same generation): no need to read
		// every node's hashes back.
		gen := bindings["gen"].(string)
		stored, cached := s.published.get(repo, gen)
		if !cached {
			var err error
			if stored, err = s.storedHashes(ctx, repo); err != nil {
				return false, fmt.Errorf("read stored hashes: %w", err)
			}
		}
		d := planDelta(stored, nodeRows, bySource)
		if d == nil {
			s.lastPublish.Store("full")
			err := s.stageAndCommit(ctx, bindings, payload, commitStage)
			if err == nil {
				s.published.put(repo, bindings["run"].(string), published)
				logx.FromContext(ctx).Info("graph published", "mode", "full", "nodes", len(nodeRows), "edges", len(edgeRows))
			}
			return true, err
		}
		d.Gen = gen
		small, err := json.Marshal(d)
		if err != nil {
			return false, err
		}
		err = s.stageAndCommit(ctx, bindings, small, commitDelta)
		if errors.Is(err, errStale) {
			return false, nil // released; a full replacement follows under a fresh lease
		}
		s.lastPublish.Store("delta")
		if err == nil {
			s.published.put(repo, bindings["run"].(string), published)
			logx.FromContext(ctx).Info("graph published", "mode", "delta", "nodes", len(d.Nodes), "gone", len(d.Gone), "edges", len(d.Edges), "unchanged", len(nodeRows)-len(d.Nodes), "hashes_cached", cached)
		}
		return true, err
	}, func(bindings map[string]any) error {
		s.lastPublish.Store("full")
		logx.FromContext(ctx).Warn("stored graph changed while publishing; writing it in full")
		err := s.stageAndCommit(ctx, bindings, payload, commitStage)
		if err == nil {
			s.published.put(repo, bindings["run"].(string), published)
		}
		return err
	})
	if err != nil {
		// An attempt may have committed before failing to report it: the
		// cache holds only what a commit confirmed.
		s.published.forget(repo)
	}
	return err
}

// underStage runs try under a fresh stage lease. When try reports it did not
// finish (false, nil), the lease is released and fallback runs under
// another. A lease left by an error is released too.
func (s *Store) underStage(ctx context.Context, repo string, try func(map[string]any) (bool, error), fallback func(map[string]any) error) error {
	attempt := func(f func(map[string]any) (bool, error)) (bool, error) {
		run, err := newRunID()
		if err != nil {
			return false, fmt.Errorf("generate ingest run: %w", err)
		}
		now := time.Now()
		bindings := map[string]any{"repo": repo, "headID": vid("ih", repo), "run": run, "now": now.UnixMilli(), "expires": now.Add(2 * time.Hour).UnixMilli(), "maxChunks": maxStageChunks}
		gen, err := s.acquire(ctx, bindings)
		if err != nil {
			return false, fmt.Errorf("reserve ingest stage: %w", err)
		}
		bindings["gen"] = gen
		done, err := f(bindings)
		if !done || err != nil {
			// Cleanup keeps the original identity but survives request cancellation.
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = s.stageWrite(cleanup, abandonStage, bindings)
		}
		return done, err
	}
	done, err := attempt(try)
	if done || err != nil {
		return err
	}
	_, err = attempt(func(b map[string]any) (bool, error) { return true, fallback(b) })
	return err
}

// stageAndCommit uploads payload in chunks under the lease in bindings and
// runs commit, which reassembles and applies it in one transaction.
func (s *Store) stageAndCommit(ctx context.Context, bindings map[string]any, payload []byte, commit string) error {
	run := bindings["run"].(string)
	chunks := (len(payload) + stageChunkBytes - 1) / stageChunkBytes
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
	b := map[string]any{}
	for key, value := range bindings {
		b[key] = value
	}
	b["chunkCount"] = chunks
	b["maxBytes"] = store.MaxReplacementBytes
	b["txCapacity"] = s.txCapacity
	b["now"] = time.Now().UnixMilli()
	if err := store.CommitCheck(ctx); err != nil {
		return err
	}
	if err := s.stageWrite(ctx, commit, b); err != nil {
		if errors.Is(err, errStale) {
			return err
		}
		return fmt.Errorf("commit ingest stage: %w", err)
	}
	return nil
}

// acquire takes the stage lease and returns the publication generation.
func (s *Store) acquire(ctx context.Context, bindings map[string]any) (string, error) {
	var result []string
	if err := s.write(ctx, acquireStage, bindings, &result); err != nil {
		return "", err
	}
	if len(result) != 2 || result[0] != "ok" {
		return "", errors.New("ingest stage rejected; retry after the active replacement completes")
	}
	return result[1], nil
}

// ErrTxCapacity reports a replacement too large for one HugeGraph
// transaction. Nothing was written.
var ErrTxCapacity = errors.New("repository replacement exceeds the HugeGraph transaction capacity")

func (s *Store) stageWrite(ctx context.Context, script string, bindings map[string]any) error {
	var result []string
	if err := s.write(ctx, script, bindings, &result); err != nil {
		return err
	}
	if len(result) == 1 && result[0] == "stale" {
		return errStale
	}
	if len(result) == 3 && result[0] == "ok" {
		return nil // a delta: ok, nodes written, nodes removed
	}
	if len(result) == 3 && result[0] == "capacity" {
		return fmt.Errorf("%w: it needs %s edge and %s vertex operations, the limit is %d; raise edge.tx_capacity and vertex.tx_capacity on the server and %s to match",
			ErrTxCapacity, result[1], result[2], s.txCapacity, "CODESYNAPSE_HUGEGRAPH_TX_CAPACITY")
	}
	if len(result) != 1 || result[0] != "ok" {
		return errors.New("ingest stage rejected; retry after the active replacement completes")
	}
	return nil
}
