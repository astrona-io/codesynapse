package hugegraphstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func node(v string, p map[string]any) map[string]any { return map[string]any{"v": v, "p": p} }

func TestPlanDelta(t *testing.T) {
	mk := func() ([]map[string]any, map[string][]map[string]any) {
		var rows []map[string]any
		for _, v := range []string{"a", "b", "c", "d", "e", "f", "h", "i", "j", "k"} {
			rows = append(rows, node(v, map[string]any{"nid": v, "name": v}))
		}
		edges := []map[string]any{{"a": "a", "b": "b", "l": "CALLS", "c": int64(1)}}
		return rows, hashRows(rows, edges)
	}
	rows, by := mk()
	stored := make([]storedHash, 0, len(rows))
	for _, r := range rows {
		p := r["p"].(map[string]any)
		stored = append(stored, storedHash{V: r["v"].(string), H: p["content_hash"].(string), E: p["edges_hash"].(string)})
	}
	assert.True(t, planDelta(nil, rows, by) == nil, "nothing stored: full replacement")

	same, sameBy := mk()
	d := planDelta(stored, same, sameBy)
	require.True(t, d != nil)
	assert.Len(t, d.Nodes, 0, "hashes are stable: an unchanged graph writes nothing")
	assert.Len(t, d.Gone, 0)

	next, _ := mk()
	next[1]["p"].(map[string]any)["name"] = "renamed"                                             // b changes
	next = append(next[:2], next[3:]...)                                                          // c is gone
	next = append(next, node("g", map[string]any{"nid": "g"}))                                    // g is new
	nextBy := hashRows(next, []map[string]any{{"a": "a", "b": "b", "l": "CALLS", "c": int64(2)}}) // a's edge changes
	d = planDelta(stored, next, nextBy)
	require.True(t, d != nil)
	var written []string
	for _, n := range d.Nodes {
		written = append(written, n["v"].(string))
	}
	assert.Equal(t, written, []string{"a", "b", "g"})
	assert.Equal(t, d.Gone, []string{"c"})
	assert.Len(t, d.Edges, 1, "the changed source's edges")
	assert.True(t, d.Nodes[0]["x"] == true, "a's edges changed: rewritten")
	assert.True(t, d.Nodes[1]["x"] == nil, "b only changed a property: its edges stay")
	assert.True(t, d.Nodes[2]["n"] == true && d.Nodes[0]["n"] == nil, "new vertices are marked")
	assert.Equal(t, len(d.Expect), 2)
	assert.Equal(t, d.Total, 10)

	most, _ := mk()
	for _, r := range most[:7] {
		r["p"].(map[string]any)["name"] = "x"
	}
	assert.True(t, planDelta(stored, most, hashRows(most, nil)) == nil, "most nodes changed: full replacement")
}

func liveStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	s, err := New(context.Background(), Config{URL: url, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD"), TxCapacity: testTxCapacity()})
	require.NoError(t, err)
	require.NoError(t, s.EnsureSchema(context.Background()))
	return s
}

func smallGraph(repo string, n int, label string) *model.Graph {
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})
	for i := range n {
		name := "f" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, name), Kind: model.KindFunction, Repo: repo, Name: name, Summary: label})
	}
	return g
}

func TestDeltaPublication(t *testing.T) {
	s := liveStore(t)
	ctx := context.Background()
	const repo = "delta-test"
	t.Cleanup(func() { _ = s.DeleteRepo(ctx, repo) })
	require.NoError(t, s.ReplaceRepo(ctx, repo, smallGraph(repo, 40, "one")))
	assert.Equal(t, s.lastPublish.Load(), "full", "nothing stored yet")
	g := smallGraph(repo, 40, "one")
	n, _ := g.Node(model.NodeID(repo, model.KindFunction, "faa"))
	n.Summary = "changed"
	g.AddNode(*n)
	require.NoError(t, s.ReplaceRepo(ctx, repo, g))
	assert.Equal(t, s.lastPublish.Load(), "delta")
	got, err := s.Node(ctx, n.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Summary, "changed")
	require.NoError(t, s.ReplaceRepo(ctx, repo, smallGraph(repo, 40, "two")))
	assert.Equal(t, s.lastPublish.Load(), "full", "every node changed")

	// A delta built on hashes that no longer match is refused and writes nothing.
	err = s.underStage(ctx, repo, func(b map[string]any) (bool, error) {
		d := &delta{Nodes: []map[string]any{node(nodeVID(n.ID), map[string]any{"nid": n.ID, "repo": repo, "kind": "Function", "name": "x"})},
			Edges: []map[string]any{}, Gone: []string{}, Expect: map[string]string{nodeVID(n.ID): "wrong|wrong"}, Total: 41}
		raw, _ := json.Marshal(d)
		return true, s.stageAndCommit(ctx, b, raw, commitDelta)
	}, nil)
	assert.ErrorIs(t, err, errStale)
	got, err = s.Node(ctx, n.ID)
	require.NoError(t, err)
	assert.Equal(t, got.Summary, "two")
	assert.Equal(t, got.Name, "faa")
}

// TestPublicationCache: a process that published a repository last reuses
// the hashes it committed instead of reading them back; another process
// publishing in between, or a deletion, makes it read them again, and the
// result is always the graph published last.
func TestPublicationCache(t *testing.T) {
	s, other := liveStore(t), liveStore(t)
	ctx := context.Background()
	const repo = "cache-test"
	t.Cleanup(func() { _ = s.DeleteRepo(ctx, repo) })
	_ = s.DeleteRepo(ctx, repo)
	publish := func(st *Store, label string) {
		t.Helper()
		g := smallGraph(repo, 40, "base")
		n, _ := g.Node(model.NodeID(repo, model.KindFunction, "faa"))
		n.Summary = label
		g.AddNode(*n)
		require.NoError(t, st.ReplaceRepo(ctx, repo, g))
		got, err := s.Node(ctx, n.ID)
		require.NoError(t, err)
		assert.Equal(t, got.Summary, label)
	}
	publish(s, "one")
	assert.Equal(t, s.hashReads.Load(), int64(1), "the first publication reads (nothing stored)")
	publish(s, "two")
	assert.Equal(t, s.lastPublish.Load(), "delta")
	assert.Equal(t, s.hashReads.Load(), int64(1), "a republish by the same process reads nothing back")

	publish(other, "three")
	assert.Equal(t, other.hashReads.Load(), int64(1), "another process has no cache")
	publish(s, "four")
	assert.Equal(t, s.hashReads.Load(), int64(2), "someone else published in between: read again")
	assert.Equal(t, s.lastPublish.Load(), "delta")

	require.NoError(t, other.DeleteRepo(ctx, repo))
	publish(s, "five")
	assert.Equal(t, s.lastPublish.Load(), "full", "deleted in between: the cached hashes are not trusted")
	assert.Equal(t, s.hashReads.Load(), int64(3))
}

// TestDeltaTiming publishes this repository, then again with one file's
// worth of change, and logs both (CODESYNAPSE_TEST_DELTA_TIMING=1).
func TestDeltaTiming(t *testing.T) {
	if os.Getenv("CODESYNAPSE_TEST_DELTA_TIMING") == "" {
		t.Skip("CODESYNAPSE_TEST_DELTA_TIMING not set")
	}
	s := liveStore(t)
	ctx := context.Background()
	root, _ := filepath.Abs("../../..")
	g, err := ingest.Run(ctx, ingest.Options{Repo: "timing", Root: root, CommitLimit: 200})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.DeleteRepo(ctx, "timing") })
	t0 := time.Now()
	require.NoError(t, s.ReplaceRepo(ctx, "timing", g))
	first := time.Since(t0)
	edited := 0
	for _, n := range g.Nodes() {
		if n.File == "internal/store/hugegraphstore/delta.go" {
			stored, _ := g.Node(n.ID) // AddNode keeps an existing node: edit it in place
			stored.Summary += " (edited)"
			edited++
		}
	}
	t1 := time.Now()
	require.NoError(t, s.ReplaceRepo(ctx, "timing", g))
	again := time.Since(t1)
	t.Logf("nodes=%d edges=%d first=%s (%v) oneFile=%s (%v, %d nodes edited)", len(g.Nodes()), len(g.Edges()), first.Round(time.Millisecond), "full", again.Round(time.Millisecond), s.lastPublish.Load(), edited)
	// The fixed cost: an unchanged republish with the hashes this process
	// committed, and with them read back from the server.
	t2 := time.Now()
	require.NoError(t, s.ReplaceRepo(ctx, "timing", g))
	cached := time.Since(t2)
	s.published.forget("timing")
	t3 := time.Now()
	require.NoError(t, s.ReplaceRepo(ctx, "timing", g))
	t.Logf("unchanged republish: %s with cached hashes, %s reading them back", cached.Round(time.Millisecond), time.Since(t3).Round(time.Millisecond))
	nodes, _, err := store.Replacement(ctx, "timing", g)
	require.NoError(t, err)
	for _, n := range nodes[:50] {
		got, err := s.Node(ctx, n.ID)
		require.NoError(t, err)
		assert.Equal(t, got.Summary, n.Summary)
	}
}
