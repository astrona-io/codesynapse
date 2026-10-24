package hugegraphstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Runtime facts (internal/runtimefacts) are their own labels, so repository
// replacement never touches them. One namespace's facts are a gzipped JSON
// document cut into chunks, because a property holds a bounded value: the
// chunks of a new generation are written first, one request each, then one
// request points the head at that generation and drops the older chunks.
// Readers follow the head only, so a half-written upload is never seen.

const (
	runtimeChunkBytes = 24 << 10
	// maxRuntimeBytes bounds the compressed document of one namespace.
	maxRuntimeBytes  = 8 << 20
	maxRuntimeChunks = (maxRuntimeBytes + runtimeChunkBytes - 1) / runtimeChunkBytes
)

const runtimeSchema = `def s=graph.schema()
['rt_ns','rt_graph','rt_gen','rt_data'].each{s.propertyKey(it).asText().ifNotExist().create()}
['rt_index','rt_count'].each{s.propertyKey(it).asLong().ifNotExist().create()}
s.vertexLabel('runtime_head').useCustomizeStringId().properties('rt_ns','rt_graph','rt_gen','rt_count').ifNotExist().create()
s.vertexLabel('runtime_chunk').useCustomizeStringId().properties('rt_ns','rt_graph','rt_gen','rt_index','rt_data').ifNotExist().create()
s.indexLabel('runtimeHeadByGraph').onV('runtime_head').by('rt_graph').secondary().ifNotExist().create()
s.indexLabel('runtimeChunkByNs').onV('runtime_chunk').by('rt_ns').secondary().ifNotExist().create()
s.indexLabel('runtimeChunkByGraph').onV('runtime_chunk').by('rt_graph').secondary().ifNotExist().create()
1`

const (
	addRuntimeChunk = `def old=g.V(v).tryNext()
if(old.isPresent()){old.get().remove()}
graph.addVertex(T.label,'runtime_chunk',T.id,v,'rt_ns',ns,'rt_graph',graphName,'rt_gen',gen,'rt_index',idx,'rt_data',data)
1`
	commitRuntime = `def chunks=g.V().hasLabel('runtime_chunk').has('rt_ns',ns).limit(maxChunks*2+2).toList()
def mine=chunks.findAll{it.value('rt_gen')==gen}
if(mine.size()!=count)return ['incomplete']
def head=g.V(headID).tryNext()
if(head.isPresent()){head.get().remove()}
graph.addVertex(T.label,'runtime_head',T.id,headID,'rt_ns',ns,'rt_graph',graphName,'rt_gen',gen,'rt_count',count)
chunks.findAll{it.value('rt_gen')!=gen}.each{it.remove()}
return ['ok']`
	readRuntime = `def head=g.V(headID).tryNext()
if(!head.isPresent())return []
def gen=head.get().value('rt_gen')
def count=head.get().value('rt_count')
def chunks=g.V().hasLabel('runtime_chunk').has('rt_ns',ns).limit(maxChunks*2+2).toList().findAll{it.value('rt_gen')==gen}.sort{it.value('rt_index')}
if(chunks.size()!=count)return ['incomplete']
chunks.collect{it.value('rt_data')}`
	deleteRuntime = `g.V().hasLabel('runtime_chunk').has('rt_ns',ns).limit(maxChunks*2+2).drop().iterate()
g.V(headID).drop().iterate()
1`
	deleteRuntimeOfGraph = `g.V().hasLabel('runtime_chunk').has('rt_graph',graphName).drop().iterate()
g.V().hasLabel('runtime_head').has('rt_graph',graphName).drop().iterate()
1`
)

// errRuntimeIncomplete reports a document whose chunks do not match its head.
var errRuntimeIncomplete = errors.New("runtime facts: incomplete document")

func runtimeHeadID(ns string) string { return vid("rth", ns) }

// SetRuntimeFacts implements store.RuntimeStore.
func (s *Store) SetRuntimeFacts(ctx context.Context, ns string, f runtimefacts.Facts) error {
	doc, err := json.Marshal(f)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(doc); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if buf.Len() > maxRuntimeBytes {
		return fmt.Errorf("runtime facts: %d bytes compressed, more than %d: %w", buf.Len(), maxRuntimeBytes, runtimefacts.ErrTooLarge)
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	gen, graphName, data := hex.EncodeToString(raw), store.RuntimeGraph(ns), buf.Bytes()
	count := 0
	for off := 0; off < len(data) || count == 0; off += runtimeChunkBytes {
		part := data[off:min(off+runtimeChunkBytes, len(data))]
		if err := s.write(ctx, addRuntimeChunk, map[string]any{
			"v": vid("rtc", fmt.Sprintf("%s|%s|%d", ns, gen, count)), "ns": ns, "graphName": graphName, "gen": gen,
			"idx": count, "data": base64.StdEncoding.EncodeToString(part),
		}, nil); err != nil {
			return err
		}
		count++
	}
	var res []string
	if err := s.write(ctx, commitRuntime, map[string]any{"ns": ns, "graphName": graphName, "gen": gen, "count": count,
		"headID": runtimeHeadID(ns), "maxChunks": maxRuntimeChunks}, &res); err != nil {
		return err
	}
	if len(res) != 1 || res[0] != "ok" {
		return errRuntimeIncomplete
	}
	return nil
}

// RuntimeFacts implements store.RuntimeReader.
func (s *Store) RuntimeFacts(ctx context.Context, ns string) (runtimefacts.Facts, error) {
	var parts []string
	if err := s.read(ctx, readRuntime, map[string]any{"ns": ns, "headID": runtimeHeadID(ns), "maxChunks": maxRuntimeChunks}, &parts); err != nil {
		return runtimefacts.Facts{}, err
	}
	if len(parts) == 0 {
		return runtimefacts.Facts{}, store.ErrNotFound
	}
	if parts[0] == "incomplete" {
		return runtimefacts.Facts{}, errRuntimeIncomplete
	}
	var data []byte
	for _, p := range parts {
		b, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			return runtimefacts.Facts{}, fmt.Errorf("runtime facts: %w", err)
		}
		data = append(data, b...)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return runtimefacts.Facts{}, fmt.Errorf("runtime facts: %w", err)
	}
	var f runtimefacts.Facts
	if err := json.NewDecoder(io.LimitReader(zr, 16*maxRuntimeBytes)).Decode(&f); err != nil {
		return runtimefacts.Facts{}, fmt.Errorf("runtime facts: %w", err)
	}
	return f, nil
}

// DeleteRuntimeFacts implements store.RuntimeStore.
func (s *Store) DeleteRuntimeFacts(ctx context.Context, ns string) error {
	return s.write(ctx, deleteRuntime, map[string]any{"ns": ns, "headID": runtimeHeadID(ns), "maxChunks": maxRuntimeChunks}, nil)
}

// deleteRuntimeOf drops every namespace's facts of graph.
func (s *Store) deleteRuntimeOf(ctx context.Context, graphName string) error {
	return s.write(ctx, deleteRuntimeOfGraph, map[string]any{"graphName": graphName}, nil)
}
