package nebulastore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

const (
	vertexBatch = 100
	edgeBatch   = 400
	deleteBatch = 400
	leaseTTL    = 2 * time.Hour
)

var _ store.Writer = (*Store)(nil)

// errBusy means another process is publishing the repository.
var errBusy = errors.New("another ingest is publishing this repository; retry after it completes")

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ReplaceRepo makes the repository's graph exactly g. NebulaGraph has no
// multi-statement transaction, so the new graph is upserted in place under a
// lease and stamped with a run; whatever carries another run is removed after
// the last batch. Everything that can fail on the data (validation, the commit
// check) happens before the first write; a failure after it leaves a mix of
// the two graphs that the next publication repairs.
func (s *Store) ReplaceRepo(ctx context.Context, repo string, g *model.Graph) error {
	nodes, edges, err := store.Replacement(ctx, repo, g)
	if err != nil {
		return err
	}
	run, err := newRunID()
	if err != nil {
		return fmt.Errorf("generate ingest run: %w", err)
	}
	head := vid("h", repo)
	if err := s.lease(ctx, head, repo, run, leaseTTL); err != nil {
		return err
	}
	// The lease is released even when the request was cancelled.
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.release(ctx, head, run)
	}()
	if err := store.CommitCheck(ctx); err != nil {
		return err
	}
	if err := s.putNodes(ctx, run, nodes); err != nil {
		return fmt.Errorf("write nodes: %w", err)
	}
	if err := s.putEdges(ctx, run, edges); err != nil {
		return fmt.Errorf("write edges: %w", err)
	}
	removed, err := s.dropStale(ctx, repo, run)
	if err != nil {
		return fmt.Errorf("remove stale graph: %w", err)
	}
	logx.FromContext(ctx).Info("graph published", "nodes", len(nodes), "edges", len(edges), "removed", removed)
	return nil
}

const (
	acquireLease = "UPSERT VERTEX ON `ingest_head` %s SET `repo` = $repo, `stage_run` = $run, `stage_expires` = $expires " +
		"WHEN `stage_run` IS NULL OR `stage_run` == \"\" OR `stage_expires` < $now"
	readLease    = "FETCH PROP ON `ingest_head` %s YIELD properties(vertex).stage_run AS run"
	releaseLease = "UPSERT VERTEX ON `ingest_head` %s SET `stage_run` = \"\", `stage_expires` = 0 WHEN `stage_run` == $run"
)

// lease takes the repository's publication lease. UPSERT with WHEN is one
// atomic statement on the vertex's partition, so two publishers cannot both
// pass. A WHEN that is false answers "filter out", which means busy.
func (s *Store) lease(ctx context.Context, head, repo, run string, ttl time.Duration) error {
	now := time.Now()
	_, err := s.write(ctx, fmt.Sprintf(acquireLease, literal(head)), map[string]any{
		"repo": repo, "run": run, "expires": now.Add(ttl).UnixMilli(), "now": now.UnixMilli(),
	})
	if err != nil {
		var ne *Error
		if errors.As(err, &ne) && strings.Contains(strings.ToLower(ne.Message), "filter") {
			return errBusy
		}
		return fmt.Errorf("reserve ingest: %w", err)
	}
	rows, err := s.write(ctx, fmt.Sprintf(readLease, literal(head)), nil)
	if err != nil {
		return fmt.Errorf("reserve ingest: %w", err)
	}
	if len(rows) != 1 || rows[0]["run"] != run {
		return errBusy
	}
	return nil
}

func (s *Store) release(ctx context.Context, head, run string) error {
	_, err := s.write(ctx, fmt.Sprintf(releaseLease, literal(head)), map[string]any{"run": run})
	return err
}

// nodeColumns is every node column in the schema order, as the INSERT lists
// them. Rows are grouped by the columns they set so unset ones stay NULL.
func nodeRow(n model.Node, run string) map[string]any {
	p := nodeprops.To(n)
	p["nid"] = p["id"]
	delete(p, "id")
	p["run"] = run
	for _, k := range nodeprops.Lists {
		if l, ok := p[k].([]string); ok {
			b, _ := json.Marshal(l) //nolint:errcheck // a []string always marshals
			p[k] = string(b)
		}
	}
	return p
}

// putNodes upserts nodes in batches that share their set columns.
func (s *Store) putNodes(ctx context.Context, run string, nodes []model.Node) error {
	type group struct {
		cols []string
		rows [][]any
		vids []string
	}
	groups := map[string]*group{}
	var order []string
	for _, n := range nodes {
		p := nodeRow(n, run)
		cols := make([]string, 0, len(p))
		for k := range p {
			cols = append(cols, k)
		}
		slices.Sort(cols)
		key := strings.Join(cols, ",")
		gr := groups[key]
		if gr == nil {
			gr = &group{cols: cols}
			groups[key] = gr
			order = append(order, key)
		}
		row := make([]any, len(cols))
		for i, c := range cols {
			row[i] = p[c]
		}
		gr.rows = append(gr.rows, row)
		gr.vids = append(gr.vids, nodeVID(n.ID))
	}
	for _, key := range order {
		gr := groups[key]
		names := make([]string, len(gr.cols))
		for i, c := range gr.cols {
			names[i] = quote(c)
		}
		for off := 0; off < len(gr.rows); off += vertexBatch {
			end := min(off+vertexBatch, len(gr.rows))
			params := map[string]any{}
			values := make([]string, 0, end-off)
			for r := off; r < end; r++ {
				ph := make([]string, len(gr.cols))
				for c := range gr.cols {
					name := fmt.Sprintf("v%d_%d", r-off, c)
					params[name] = gr.rows[r][c]
					ph[c] = "$" + name
				}
				values = append(values, literal(gr.vids[r])+":("+strings.Join(ph, ",")+")")
			}
			stmt := "INSERT VERTEX `node` (" + strings.Join(names, ",") + ") VALUES " + strings.Join(values, ",")
			if _, err := s.write(ctx, stmt, params); err != nil {
				return err
			}
		}
	}
	return nil
}

// putEdges upserts edges by type.
func (s *Store) putEdges(ctx context.Context, run string, edges []model.Edge) error {
	byKind := map[model.EdgeKind][]model.Edge{}
	var kinds []model.EdgeKind
	for _, e := range edges {
		if !e.Kind.Valid() {
			return fmt.Errorf("invalid edge kind %q", e.Kind)
		}
		if _, ok := byKind[e.Kind]; !ok {
			kinds = append(kinds, e.Kind)
		}
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	for _, kind := range kinds {
		list := byKind[kind]
		for off := 0; off < len(list); off += edgeBatch {
			end := min(off+edgeBatch, len(list))
			params := map[string]any{"run": run}
			values := make([]string, 0, end-off)
			for i, e := range list[off:end] {
				c, ev := fmt.Sprintf("c%d", i), fmt.Sprintf("e%d", i)
				params[c], params[ev] = int64(max(e.Count, 1)), string(e.Evidence)
				values = append(values, literal(nodeVID(e.From))+"->"+literal(nodeVID(e.To))+"@0:($"+c+",$run,$"+ev+")")
			}
			stmt := "INSERT EDGE " + quote(string(kind)) + " (`count`,`run`,`evidence`) VALUES " + strings.Join(values, ",")
			if _, err := s.write(ctx, stmt, params); err != nil {
				return err
			}
		}
	}
	return nil
}

// dropStale removes the repository's nodes and edges that another run wrote.
func (s *Store) dropStale(ctx context.Context, repo, run string) (int, error) {
	const staleNodes = "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo AND v.`node`.`run` != $run RETURN id(v) AS id LIMIT 400"
	removed := 0
	for {
		rows, err := s.read(ctx, staleNodes, map[string]any{"repo": repo, "run": run})
		if err != nil {
			return removed, err
		}
		if len(rows) == 0 {
			break
		}
		ids := make([]string, len(rows))
		for i, r := range rows {
			ids[i], _ = r["id"].(string)
		}
		if _, err := s.write(ctx, "DELETE VERTEX "+literals(ids)+" WITH EDGE", nil); err != nil {
			return removed, err
		}
		removed += len(ids)
	}
	return removed, s.dropStaleEdges(ctx, repo, run)
}

// dropStaleEdges removes edges of kept nodes that this run did not write.
func (s *Store) dropStaleEdges(ctx context.Context, repo, run string) error {
	const stale = "MATCH (a:`node`)-[e]->(b) WHERE a.`node`.`repo` == $repo AND e.`run` != $run " +
		"RETURN id(a) AS a, id(b) AS b, type(e) AS t LIMIT 400"
	for {
		rows, err := s.read(ctx, stale, map[string]any{"repo": repo, "run": run})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
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
		for t, pairs := range byKind {
			if _, err := s.write(ctx, "DELETE EDGE "+quote(t)+" "+strings.Join(pairs, ","), nil); err != nil {
				return err
			}
		}
	}
}

// DeleteRepo implements store.Writer.
func (s *Store) DeleteRepo(ctx context.Context, repo string) error {
	// Nothing of this run exists, so every node and edge of the repository is stale.
	run, err := newRunID()
	if err != nil {
		return err
	}
	if _, err := s.dropStale(ctx, repo, run); err != nil {
		return err
	}
	return s.deleteRuntimeOf(ctx, repo)
}
