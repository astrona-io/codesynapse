package nebulastore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

// List implements store.Reader. Filters run in the database; metrics that
// need edges (fan-in, authored) are counted by one aggregate query and the
// ranking is finished here.
func (s *Store) List(ctx context.Context, repo string, f store.Filter, m store.Metric, limit int) ([]store.Ranked, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("unknown metric %q", m)
	}
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	limit = store.ClampLimit(limit, 20)
	where := "v.`node`.`repo` == $repo AND (size($kinds) == 0 OR v.`node`.`kind` IN $kinds)"
	params := map[string]any{"repo": repo, "kinds": anys(kindStrings(f.Kinds)), "limit": int64(limit)}
	if f.Tag != "" {
		// Lists are stored as JSON text, so the tag is matched as its JSON string.
		quoted, _ := json.Marshal(f.Tag) //nolint:errcheck // a string always marshals
		where += " AND coalesce(v.`node`.`security_tags`, \"\") CONTAINS $tag"
		params["tag"] = string(quoted)
	}
	if f.FilePrefix != "" {
		where += " AND coalesce(v.`node`.`file`, \"\") STARTS WITH $prefix"
		params["prefix"] = f.FilePrefix
	}
	if f.ExportedOnly {
		where += " AND coalesce(v.`node`.`exported`, false)"
	}
	if f.Undocumented {
		where += " AND coalesce(v.`node`.`summary`, \"\") == \"\""
	}
	if f.ExcludeTests {
		where += " AND NOT coalesce(v.`node`.`is_test`, false)"
	}
	match := "MATCH (v:`node`) WHERE " + where

	var order, dir string
	switch m {
	case store.ByComplexity:
		order, dir = "coalesce(v.`node`.`complexity`, 0)", " DESC"
	case store.ByChurn:
		order, dir = "coalesce(v.`node`.`churn`, 0)", " DESC"
	case store.ByLOC:
		order, dir = "coalesce(v.`node`.`loc`, 0)", " DESC"
	case store.ByTime:
		order, dir = "coalesce(v.`node`.`time`, \"\")", " DESC"
	case store.ByName:
		order = "CASE WHEN coalesce(v.`node`.`qualified_name`, \"\") != \"\" THEN v.`node`.`qualified_name` ELSE coalesce(v.`node`.`name`, \"\") END"
	}
	if order != "" {
		rows, err := s.read(ctx, match+" RETURN properties(v) AS p, "+order+" AS sort, v.`node`.`nid` AS nid ORDER BY sort"+dir+", nid LIMIT $limit", params)
		if err != nil {
			return nil, err
		}
		out := make([]store.Ranked, 0, len(rows))
		for _, n := range nodesOf(rows) {
			out = append(out, store.Ranked{Node: n, Score: scoreOf(m, n)})
		}
		return out, nil
	}

	// fan_in and authored: count edges, then rank the candidates.
	edge, side := "CALLS", "b"
	if m == store.ByAuthored {
		edge, side = "AUTHORED", "a"
	}
	cand, err := s.read(ctx, match+" RETURN id(v) AS id, v.`node`.`nid` AS nid", params)
	if err != nil {
		return nil, err
	}
	counts, err := s.read(ctx, "MATCH (a:`node`)-[e:"+quote(edge)+"]->(b:`node`) WHERE "+strings.ReplaceAll(where, "v.", side+".")+
		" RETURN id("+side+") AS id, count(e) AS n", params)
	if err != nil {
		return nil, err
	}
	score := map[string]int{}
	for _, r := range counts {
		id, _ := r["id"].(string)
		score[id] = nodeprops.Int(r["n"])
	}
	type ranked struct {
		vid, nid string
		score    int
	}
	all := make([]ranked, 0, len(cand))
	for _, r := range cand {
		id, _ := r["id"].(string)
		nid, _ := r["nid"].(string)
		all = append(all, ranked{id, nid, score[id]})
	}
	slices.SortFunc(all, func(a, b ranked) int { return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.nid, b.nid)) })
	all = all[:min(len(all), limit)]
	vids := make([]string, len(all))
	for i, r := range all {
		vids[i] = r.vid
	}
	nodes, err := s.fetchNodes(ctx, vids)
	if err != nil {
		return nil, err
	}
	out := make([]store.Ranked, 0, len(all))
	for _, r := range all {
		if n, ok := nodes[r.vid]; ok {
			out = append(out, store.Ranked{Node: n, Score: r.score})
		}
	}
	return out, nil
}

func scoreOf(m store.Metric, n model.Node) int {
	switch m {
	case store.ByComplexity:
		return n.Complexity
	case store.ByChurn:
		return n.Churn
	case store.ByLOC:
		return n.LOC
	}
	return 0
}

// DependencyGraph implements store.Reader.
func (s *Store) DependencyGraph(ctx context.Context, repo string) (store.DependencyGraph, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return store.DependencyGraph{}, err
	}
	g := store.DependencyGraph{}
	kind := func(k string) ([]model.Node, bool, error) {
		rows, err := s.read(ctx, "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo AND v.`node`.`kind` == $kind"+
			" RETURN properties(v) AS p, v.`node`.`nid` AS nid ORDER BY nid LIMIT $limit",
			map[string]any{"repo": repo, "kind": k, "limit": int64(store.MaxDependencies + 1)})
		if err != nil {
			return nil, false, err
		}
		ns := nodesOf(rows)
		return ns[:min(len(ns), store.MaxDependencies)], len(ns) > store.MaxDependencies, nil
	}
	var more bool
	var err error
	if g.Modules, more, err = kind("Module"); err != nil {
		return g, err
	}
	g.Truncated = more
	var vmore bool
	if g.Vulnerabilities, vmore, err = kind("Vulnerability"); err != nil {
		return g, err
	}
	g.Truncated = g.Truncated || vmore
	rows, err := s.read(ctx, "MATCH (a:`node`)-[:`DEPENDS_ON`]->(b:`node`) WHERE a.`node`.`repo` == $repo AND a.`node`.`kind` == \"Module\""+
		" AND b.`node`.`kind` == \"Module\" RETURN a.`node`.`nid` AS a, b.`node`.`nid` AS b ORDER BY a, b LIMIT $limit",
		map[string]any{"repo": repo, "limit": int64(store.MaxDependencyEdges + 1)})
	if err != nil {
		return g, err
	}
	if len(rows) > store.MaxDependencyEdges {
		rows, g.Truncated = rows[:store.MaxDependencyEdges], true
	}
	for _, r := range rows {
		a, _ := r["a"].(string)
		b, _ := r["b"].(string)
		g.Edges = append(g.Edges, model.Edge{From: a, To: b, Kind: model.EdgeDependsOn, Count: 1})
	}
	return g, nil
}

// Subgraph implements store.Reader.
func (s *Store) Subgraph(ctx context.Context, ids []string, kinds []model.EdgeKind) ([]model.Edge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > store.MaxLimit*4 {
		return nil, fmt.Errorf("subgraph accepts at most %d ids", store.MaxLimit*4)
	}
	over := "*"
	if len(kinds) > 0 {
		types := make([]string, 0, len(kinds))
		for _, k := range kinds {
			if !k.Valid() {
				return nil, fmt.Errorf("invalid edge kind %q", k)
			}
			types = append(types, quote(string(k)))
		}
		over = strings.Join(types, ",")
	}
	byVID := make(map[string]string, len(ids))
	vids := make([]string, 0, len(ids))
	for _, id := range ids {
		v := nodeVID(id)
		if _, dup := byVID[v]; !dup {
			byVID[v] = id
			vids = append(vids, v)
		}
	}
	var out []model.Edge
	for off := 0; off < len(vids); off += 200 {
		part := vids[off:min(off+200, len(vids))]
		rows, err := s.read(ctx, "GO FROM "+literals(part)+" OVER "+over+
			" YIELD src(edge) AS a, dst(edge) AS b, type(edge) AS t, properties(edge).count AS c, properties(edge).evidence AS ev", nil)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			a, _ := r["a"].(string)
			b, _ := r["b"].(string)
			t, _ := r["t"].(string)
			from, ok1 := byVID[a]
			to, ok2 := byVID[b]
			if !ok1 || !ok2 {
				continue
			}
			ev, _ := r["ev"].(string)
			out = append(out, model.Edge{From: from, To: to, Kind: model.EdgeKind(t), Count: max(nodeprops.Int(r["c"]), 1), Evidence: store.ReadEvidence(ev)})
		}
	}
	slices.SortFunc(out, func(a, b model.Edge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To), cmp.Compare(a.Kind, b.Kind))
	})
	return out[:min(len(out), store.MaxSubgraphEdges)], nil
}

// Stats implements store.Reader.
func (s *Store) Stats(ctx context.Context, repo string) (model.Stats, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return model.Stats{}, err
	}
	params := map[string]any{"repo": repo}
	const pkg = "coalesce(v.`node`.`package`, \"\")"
	groups, err := s.read(ctx, "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo RETURN v.`node`.`kind` AS kind,"+
		" coalesce(v.`node`.`is_test`, false) AS test, coalesce(v.`node`.`exported`, false) AS exported,"+
		" coalesce(v.`node`.`summary`, \"\") != \"\" AS documented, coalesce(v.`node`.`language`, \"\") AS language,"+
		" CASE WHEN v.`node`.`kind` == \"Template\" THEN "+pkg+" WHEN "+pkg+" != \"\" THEN \"x\" ELSE \"\" END AS pkg,"+
		" CASE WHEN v.`node`.`kind` == \"ConfigEntry\" THEN coalesce(v.`node`.`signature`, \"\") ELSE \"\" END AS sig,"+
		" count(v) AS n, sum(coalesce(v.`node`.`loc`, 0)) AS loc, sum(coalesce(v.`node`.`complexity`, 0)) AS cx", params)
	if err != nil {
		return model.Stats{}, err
	}
	b := store.NewStatsBuilder(repo)
	for _, r := range groups {
		str := func(k string) string { v, _ := r[k].(string); return v }
		flag := func(k string) bool { v, _ := r[k].(bool); return v }
		n := model.Node{Kind: model.NodeKind(str("kind")), IsTest: flag("test"), Exported: flag("exported"), Language: str("language"), Package: str("pkg"),
			Signature: str("sig"), LOC: nodeprops.Int(r["loc"]), Complexity: nodeprops.Int(r["cx"])}
		if flag("documented") {
			n.Summary = "x"
		}
		b.Add(n, nodeprops.Int(r["n"]))
	}
	tagRows, err := s.read(ctx, "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo AND v.`node`.`kind` IN [\"Function\",\"Method\"]"+
		" AND NOT coalesce(v.`node`.`is_test`, false) AND v.`node`.`security_tags` IS NOT NULL RETURN v.`node`.`security_tags` AS tg", params)
	if err != nil {
		return model.Stats{}, err
	}
	tags := map[string]int{}
	for _, r := range tagRows {
		text, _ := r["tg"].(string)
		var list []string
		if json.Unmarshal([]byte(text), &list) != nil {
			return model.Stats{}, errors.New("nebula: unreadable security tags")
		}
		for _, t := range list {
			tags[t]++
		}
	}
	b.AddTags(tags)
	edgeRows, err := s.read(ctx, "MATCH (a:`node`)-[e]->() WHERE a.`node`.`repo` == $repo RETURN type(e) AS t, count(e) AS n", params)
	if err != nil {
		return model.Stats{}, err
	}
	for _, r := range edgeRows {
		t, _ := r["t"].(string)
		b.AddEdges(model.EdgeKind(t), nodeprops.Int(r["n"]))
	}
	return b.Stats(), nil
}
