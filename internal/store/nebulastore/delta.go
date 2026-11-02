package nebulastore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// Incremental publication. Every node vertex carries content_hash (its
// properties) and edges_hash (its outgoing edges), so a publication writes
// only new and changed nodes, the outgoing edges of nodes whose edges
// changed, and removals. A stored hash is written only after the node's
// outgoing edges are exactly what it describes (edges go first), so a
// publication that stops halfway leaves hashes that no longer match and the
// next one rewrites those nodes.

// fullShare is the share of changed and removed nodes above which a full
// write is cheaper than the delta's reads and targeted deletes.
const fullShare = 2

// nodeHash is what a node vertex records about itself.
type nodeHash struct{ content, edges string }

// delta is what one publication writes.
type delta struct {
	full     bool
	nodes    []model.Node // new and changed nodes
	edgeFrom []string     // vertex IDs whose outgoing edges are rewritten
	edges    []model.Edge // every outgoing edge of edgeFrom
	removed  []string     // vertex IDs stored but no longer in the graph
}

// hashNodes computes the hashes of every node from the graph that will be
// stored. Hashes are over the stored form, so a change the store would not
// see (a field nodeprops drops) is no change.
func hashNodes(nodes []model.Node, edges []model.Edge) map[string]nodeHash {
	out := make(map[string][]string, len(nodes))
	for _, e := range edges {
		out[e.From] = append(out[e.From], string(e.Kind)+"\x00"+e.To+"\x00"+strconv.Itoa(max(e.Count, 1))+"\x00"+string(e.Evidence))
	}
	hashes := make(map[string]nodeHash, len(nodes))
	for _, n := range nodes {
		p := nodeRow(n, "")
		delete(p, "run")
		b, _ := json.Marshal(p) //nolint:errcheck // strings, numbers and bools always marshal; map keys are sorted
		lines := out[n.ID]
		slices.Sort(lines)
		hashes[n.ID] = nodeHash{content: digest(b), edges: digest([]byte(strings.Join(lines, "\n")))}
	}
	return hashes
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// planDelta compares the graph with what is stored (keyed by vertex ID).
// Nothing stored, or more than half of the nodes changed or removed, makes it
// a full write.
func planDelta(nodes []model.Node, edges []model.Edge, want map[string]nodeHash, stored map[string]nodeHash) delta {
	if len(stored) == 0 {
		return delta{full: true}
	}
	var d delta
	present := make(map[string]bool, len(nodes))
	rewrite := map[string]bool{}
	for _, n := range nodes {
		v := nodeVID(n.ID)
		present[v] = true
		have, ok := stored[v]
		w := want[n.ID]
		if ok && have == w {
			continue
		}
		d.nodes = append(d.nodes, n)
		if !ok || have.edges != w.edges {
			rewrite[n.ID] = true
			d.edgeFrom = append(d.edgeFrom, v)
		}
	}
	for v := range stored {
		if !present[v] {
			d.removed = append(d.removed, v)
		}
	}
	slices.Sort(d.removed)
	if fullShare*(len(d.nodes)+len(d.removed)) > len(nodes) {
		return delta{full: true}
	}
	for _, e := range edges {
		if rewrite[e.From] {
			d.edges = append(d.edges, e)
		}
	}
	return d
}

// storedHashes reads the hashes of the repository's node vertices, one
// indexed read per kind so no single answer holds the whole repository.
// Vertices written before hashes existed read as "" and count as changed.
func (s *Store) storedHashes(ctx context.Context, repo string) (map[string]nodeHash, error) {
	const stmt = "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo AND v.`node`.`kind` == $kind " +
		"RETURN id(v) AS id, v.`node`.`content_hash` AS c, v.`node`.`edges_hash` AS e"
	out := map[string]nodeHash{}
	for _, k := range model.AllNodeKinds {
		rows, err := s.read(ctx, stmt, map[string]any{"repo": repo, "kind": string(k)})
		if err != nil {
			return nil, fmt.Errorf("read stored hashes: %w", err)
		}
		for _, r := range rows {
			id, _ := r["id"].(string)
			c, _ := r["c"].(string)
			e, _ := r["e"].(string)
			if id != "" {
				out[id] = nodeHash{content: c, edges: e}
			}
		}
	}
	return out, nil
}

// publishDelta writes d: edges of the nodes whose edges changed, then drops
// their outgoing edges this run did not write, then the nodes with their
// hashes, then the removed vertices with their edges.
func (s *Store) publishDelta(ctx context.Context, run string, d delta, hashes map[string]nodeHash) error {
	if err := s.putEdges(ctx, run, d.edges); err != nil {
		return fmt.Errorf("write edges: %w", err)
	}
	if err := s.dropStaleEdgesFrom(ctx, run, d.edgeFrom); err != nil {
		return fmt.Errorf("remove changed edges: %w", err)
	}
	if err := s.putNodes(ctx, run, d.nodes, hashes); err != nil {
		return fmt.Errorf("write nodes: %w", err)
	}
	for off := 0; off < len(d.removed); off += deleteBatch {
		chunk := d.removed[off:min(off+deleteBatch, len(d.removed))]
		if _, err := s.write(ctx, "DELETE VERTEX "+literals(chunk)+" WITH EDGE", nil); err != nil {
			return fmt.Errorf("remove nodes: %w", err)
		}
	}
	return nil
}

// dropStaleEdgesFrom removes the outgoing edges of the given vertices that
// another run wrote.
func (s *Store) dropStaleEdgesFrom(ctx context.Context, run string, from []string) error {
	for off := 0; off < len(from); off += vertexBatch {
		chunk := from[off:min(off+vertexBatch, len(from))]
		// IDs reach the text only through literals, which accepts vid() output alone.
		stmt := "MATCH (a)-[e]->(b) WHERE id(a) IN [" + literals(chunk) + "] AND e.`run` != $run " +
			"RETURN id(a) AS a, id(b) AS b, type(e) AS t"
		rows, err := s.read(ctx, stmt, map[string]any{"run": run})
		if err != nil {
			return err
		}
		if err := s.deleteEdgeRows(ctx, rows); err != nil {
			return err
		}
	}
	return nil
}

// deleteEdgeRows deletes the edges named by rows of a, b and t, grouped by type.
func (s *Store) deleteEdgeRows(ctx context.Context, rows []Row) error {
	byKind := map[string][]string{}
	for _, r := range rows {
		a, _ := r["a"].(string)
		b, _ := r["b"].(string)
		t, _ := r["t"].(string)
		if !model.EdgeKind(t).Valid() {
			return fmt.Errorf("stored edge has invalid type %q", t)
		}
		byKind[t] = append(byKind[t], literal(a)+"->"+literal(b)+"@0")
	}
	kinds := make([]string, 0, len(byKind))
	for t := range byKind {
		kinds = append(kinds, t)
	}
	slices.SortFunc(kinds, cmp.Compare)
	for _, t := range kinds {
		pairs := byKind[t]
		for off := 0; off < len(pairs); off += deleteBatch {
			if _, err := s.write(ctx, "DELETE EDGE "+quote(t)+" "+strings.Join(pairs[off:min(off+deleteBatch, len(pairs))], ","), nil); err != nil {
				return err
			}
		}
	}
	return nil
}
