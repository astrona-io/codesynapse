package nebulastore

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

var (
	_ store.RuntimeStore  = (*Store)(nil)
	_ store.RuntimeReader = (*Store)(nil)
)

// Runtime facts (internal/runtimefacts) are their own tags, so repository
// replacement never touches them. One namespace's facts are a gzipped JSON
// document cut into chunks, because a property holds a bounded value: the
// chunks of a new generation are written first, one request each, then the
// head is pointed at that generation and the older chunks are dropped.
// Readers follow the head only, so a half-written upload is never seen.

const (
	runtimeChunkBytes = 24 << 10
	// maxRuntimeBytes bounds the compressed document of one namespace.
	maxRuntimeBytes  = 8 << 20
	maxRuntimeChunks = (maxRuntimeBytes + runtimeChunkBytes - 1) / runtimeChunkBytes
)

// errRuntimeIncomplete reports a document whose chunks do not match its head.
var errRuntimeIncomplete = errors.New("runtime facts: incomplete document")

func runtimeHeadVID(ns string) string { return vid("h", "runtime|"+ns) }

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
		if _, err := s.write(ctx, "INSERT VERTEX `runtime_chunk` (`ns`,`graph_name`,`gen`,`idx`,`data`) VALUES "+literal(vid("k", fmt.Sprintf("%s|%s|%d", ns, gen, count)))+":($ns,$graph,$gen,$idx,$data)",
			map[string]any{"ns": ns, "graph": graphName, "gen": gen, "idx": int64(count), "data": base64.StdEncoding.EncodeToString(part)}); err != nil {
			return err
		}
		count++
	}
	have, err := s.read(ctx, "MATCH (v:`runtime_chunk`) WHERE v.`runtime_chunk`.`ns` == $ns AND v.`runtime_chunk`.`gen` == $gen RETURN count(v) AS n", map[string]any{"ns": ns, "gen": gen})
	if err != nil {
		return err
	}
	if len(have) != 1 || have[0]["n"] != int64(count) {
		return errRuntimeIncomplete
	}
	if _, err := s.write(ctx, "INSERT VERTEX `runtime_head` (`ns`,`graph_name`,`gen`,`chunks`) VALUES "+literal(runtimeHeadVID(ns))+":($ns,$graph,$gen,$chunks)",
		map[string]any{"ns": ns, "graph": graphName, "gen": gen, "chunks": int64(count)}); err != nil {
		return err
	}
	return s.dropChunks(ctx, "MATCH (v:`runtime_chunk`) WHERE v.`runtime_chunk`.`ns` == $ns AND v.`runtime_chunk`.`gen` != $gen RETURN id(v) AS id LIMIT 400", map[string]any{"ns": ns, "gen": gen})
}

// dropChunks deletes what the constant statement selects, in batches.
func (s *Store) dropChunks(ctx context.Context, stmt string, params map[string]any) error {
	for {
		rows, err := s.read(ctx, stmt, params)
		if err != nil || len(rows) == 0 {
			return err
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i], _ = r["id"].(string)
		}
		if err := s.deleteVertices(ctx, ids); err != nil {
			return err
		}
	}
}

// RuntimeFacts implements store.RuntimeReader.
func (s *Store) RuntimeFacts(ctx context.Context, ns string) (runtimefacts.Facts, error) {
	head, err := s.read(ctx, "FETCH PROP ON `runtime_head` "+literal(runtimeHeadVID(ns))+" YIELD properties(vertex).gen AS gen, properties(vertex).chunks AS chunks", nil)
	if err != nil {
		return runtimefacts.Facts{}, err
	}
	if len(head) == 0 {
		return runtimefacts.Facts{}, store.ErrNotFound
	}
	gen, _ := head[0]["gen"].(string)
	count, _ := head[0]["chunks"].(int64)
	rows, err := s.read(ctx, "MATCH (v:`runtime_chunk`) WHERE v.`runtime_chunk`.`ns` == $ns AND v.`runtime_chunk`.`gen` == $gen RETURN v.`runtime_chunk`.`idx` AS idx, v.`runtime_chunk`.`data` AS data ORDER BY idx LIMIT $limit",
		map[string]any{"ns": ns, "gen": gen, "limit": int64(maxRuntimeChunks + 1)})
	if err != nil {
		return runtimefacts.Facts{}, err
	}
	if int64(len(rows)) != count {
		return runtimefacts.Facts{}, errRuntimeIncomplete
	}
	var data []byte
	for _, r := range rows {
		p, _ := r["data"].(string)
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
	if err := s.deleteVertices(ctx, []string{runtimeHeadVID(ns)}); err != nil {
		return err
	}
	return s.dropChunks(ctx, "MATCH (v:`runtime_chunk`) WHERE v.`runtime_chunk`.`ns` == $ns RETURN id(v) AS id LIMIT 400", map[string]any{"ns": ns})
}

// deleteRuntimeOf drops every namespace's facts of graph.
func (s *Store) deleteRuntimeOf(ctx context.Context, graphName string) error {
	if err := s.dropChunks(ctx, "MATCH (v:`runtime_head`) WHERE v.`runtime_head`.`graph_name` == $graph RETURN id(v) AS id LIMIT 400", map[string]any{"graph": graphName}); err != nil {
		return err
	}
	return s.dropChunks(ctx, "MATCH (v:`runtime_chunk`) WHERE v.`runtime_chunk`.`graph_name` == $graph RETURN id(v) AS id LIMIT 400", map[string]any{"graph": graphName})
}
