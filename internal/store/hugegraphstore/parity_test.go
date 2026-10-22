package hugegraphstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
)

// TestParityWithReference indexes this repository and checks that HugeGraph
// answers real queries exactly like the in-memory reference store.
func TestParityWithReference(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	if raceEnabled {
		// Indexing a real module trips a data race inside go/packages'
		// concurrent type-checking (x/tools v0.52.0), not in this package.
		t.Skip("skipped under -race: upstream go/packages race while indexing a real module")
	}
	ctx := context.Background()
	root, _ := filepath.Abs("../../..")
	g, err := ingest.Run(ctx, ingest.Options{Repo: "parity", Root: root, CommitLimit: 200})
	if err != nil {
		t.Fatal(err)
	}
	mem := memory.New()
	_ = mem.ReplaceRepo(ctx, "parity", g)
	hg, err := New(ctx, Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hg.DeleteRepo(ctx, "parity") })
	t0 := time.Now()
	if err := hg.ReplaceRepo(ctx, "parity", g); err != nil {
		t.Fatal(err)
	}
	t.Logf("replace %v", time.Since(t0))
	t0 = time.Now()
	if err := hg.ReplaceRepo(ctx, "parity", g); err != nil {
		t.Fatal(err)
	}
	t.Logf("re-replace %v", time.Since(t0))
	same := func(name string, a, b any) {
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if !reflect.DeepEqual(ja, jb) {
			t.Errorf("%s differs:\nmem %.400s\nhg  %.400s", name, ja, jb)
		}
	}
	timed := func(name string, f func()) { t0 := time.Now(); f(); t.Logf("%s %v", name, time.Since(t0)) }
	var a, b any
	timed("stats", func() { a, _ = mem.Stats(ctx, "parity"); b, _ = hg.Stats(ctx, "parity") })
	same("stats", a, b)
	timed("search", func() {
		a, _ = mem.Search(ctx, "parity", "store", nil, 50)
		b, _ = hg.Search(ctx, "parity", "store", nil, 50)
	})
	same("search", a, b)
	for _, m := range []store.Metric{store.ByComplexity, store.ByFanIn, store.ByName, store.ByTime, store.ByChurn} {
		f := store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod, model.KindCommit}, ExcludeTests: true}
		timed("list "+string(m), func() { a, _ = mem.List(ctx, "parity", f, m, 30); b, _ = hg.List(ctx, "parity", f, m, 30) })
		same("list "+string(m), a, b)
	}
	top, _ := mem.List(ctx, "parity", store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}}, store.ByFanIn, 1)
	id := top[0].Node.ID
	t.Logf("callers of %s (fan-in %d)", id, top[0].Score)
	timed("callers", func() {
		a, _ = mem.Traverse(ctx, id, []model.EdgeKind{model.EdgeCalls}, store.In, 3, 100)
		b, _ = hg.Traverse(ctx, id, []model.EdgeKind{model.EdgeCalls}, store.In, 3, 100)
	})
	same("callers", a, b)
	timed("file", func() {
		a, _ = mem.NodesInFile(ctx, "parity", "internal/store/hugegraphstore/graph.go")
		b, _ = hg.NodesInFile(ctx, "parity", "internal/store/hugegraphstore/graph.go")
	})
	same("file", a, b)
	timed("findings", func() { a, _ = mem.FindingCounts(ctx, "parity"); b, _ = hg.FindingCounts(ctx, "parity") })
	same("findings", a, b)
	if hits, _ := mem.Traverse(ctx, id, []model.EdgeKind{model.EdgeCalls}, store.In, 3, 100); len(hits) == 0 {
		t.Error("no callers found: the parity check would be vacuous")
	}
}
