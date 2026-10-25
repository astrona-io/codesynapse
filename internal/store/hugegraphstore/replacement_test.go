package hugegraphstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestContractAtomicReplacement(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	ctx := context.Background()
	s, err := New(ctx, Config{URL: url, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD"), TxCapacity: testTxCapacity()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))
	fixture := storetest.Fixture()
	large := fixture.AddNode(model.Node{ID: storetest.ID(model.KindDocument, "unicode.md"), Repo: storetest.Repo, Kind: model.KindDocument, Name: "unicode", Summary: strings.Repeat("é漢🧪", 4000)})
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, fixture))
	loaded, err := s.Node(ctx, large.ID)
	require.NoError(t, err)
	assert.Equal(t, loaded.Summary, large.Summary)
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	root, err := s.Node(ctx, storetest.ID(model.KindRepository, storetest.Repo))
	require.NoError(t, err)
	root.Head = "must-not-publish"
	p := nodeprops.To(root)
	p["nid"] = p["id"]
	delete(p, "id")
	invalid := map[string]any{"nid": storetest.ID(model.KindFile, "bad"), "repo": storetest.Repo, "kind": "File", "bytes": "wrong-property-type"}
	payload, err := json.Marshal(map[string]any{"nodes": []map[string]any{{"v": nodeVID(root.ID), "p": p}, {"v": nodeVID(storetest.ID(model.KindFile, "bad")), "p": invalid}}, "edges": []any{}})
	require.NoError(t, err)
	run, err := newRunID()
	require.NoError(t, err)
	b := map[string]any{"repo": storetest.Repo, "headID": vid("ih", storetest.Repo), "run": run, "now": time.Now().UnixMilli(), "expires": time.Now().Add(time.Hour).UnixMilli(), "maxChunks": maxStageChunks}
	_, err = s.acquire(ctx, b)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.stageWrite(ctx, abandonStage, b) })
	b["idx"] = 0
	b["chunkID"] = vid("is", run+"|0")
	b["data"] = base64.StdEncoding.EncodeToString(payload)
	require.NoError(t, s.stageWrite(ctx, uploadStage, b))
	// Staging is invisible to existing readers.
	before, err := s.Node(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, before.Head, "abc")
	b["chunkCount"] = 1
	b["maxBytes"] = store.MaxReplacementBytes
	b["txCapacity"] = s.txCapacity
	require.Error(t, s.stageWrite(ctx, commitStage, b))
	after, err := s.Node(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, after.Head, "abc")
	// Abandoning commits its own transaction: the failed replacement must
	// have been rolled back, not left for this commit to publish.
	require.NoError(t, s.stageWrite(ctx, abandonStage, b))
	after, err = s.Node(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, after.Head, "abc")
	main, err := s.Node(ctx, storetest.ID(model.KindFunction, "app.main"))
	require.NoError(t, err)
	assert.Equal(t, main.Name, "main")
	edges, err := s.Subgraph(ctx, []string{main.ID, storetest.ID(model.KindFunction, "app.Handler")}, nil)
	require.NoError(t, err)
	assert.Len(t, edges, 1)
}

// testTxCapacity is the transaction capacity the test recipe gave the server.
func testTxCapacity() int {
	capacity, _ := strconv.Atoi(os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_TX_CAPACITY"))
	return capacity
}

func TestStageSchemaUpgradesLegacyHead(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	ctx := t.Context()
	s, err := New(ctx, Config{URL: url, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))
	// Separate fixture labels exercise an older schema without touching the
	// graph's real publication heads or any indexed repository.
	const legacy = `def s=graph.schema()
s.vertexLabel('legacy_head_fixture').useCustomizeStringId().properties('repo','stage_run','stage_expires').ifNotExist().create()
graph.addVertex(T.id,'legacy-head-fixture',T.label,'legacy_head_fixture','repo','legacy-fixture','stage_run','','stage_expires',0L)
graph.tx().commit();1`
	const cleanup = `g.V('legacy-head-fixture').drop().iterate();graph.tx().commit()
graph.schema().vertexLabel('legacy_head_fixture').remove()
graph.schema().vertexLabel('legacy_stage_fixture').remove();1`
	require.NoError(t, s.write(ctx, legacy, nil, nil))
	t.Cleanup(func() { _ = s.write(context.Background(), cleanup, nil, nil) })
	upgrade := strings.NewReplacer("'ingest_head'", "'legacy_head_fixture'", "'ingest_stage'", "'legacy_stage_fixture'", "'ingestStageByRepo'", "'legacyStageByRepo'", "'ingestStageByExpiry'", "'legacyStageByExpiry'").Replace(stageSchema)
	require.NoError(t, s.write(ctx, upgrade, nil, nil))
	require.NoError(t, s.write(ctx, upgrade, nil, nil), "migration is idempotent")
	const verify = `def v=g.V('legacy-head-fixture').next()
v.property('pub_gen','generation-fixture');graph.tx().commit()
g.V('legacy-head-fixture').values('pub_gen').toList()`
	var values []string
	require.NoError(t, s.write(ctx, verify, nil, &values))
	assert.Equal(t, values, []string{"generation-fixture"}, "an existing head survives and accepts the new property")
}

func TestReplacementTxCapacity(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	ctx := context.Background()
	s, err := New(ctx, Config{URL: url, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD"), TxCapacity: testTxCapacity()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))

	small, err := New(ctx, Config{URL: url, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD"), TxCapacity: 5})
	require.NoError(t, err)
	t.Cleanup(func() { _ = small.Close(ctx) })
	// Ten new nodes do not fit five operations, even written as a delta.
	grown := storetest.Fixture()
	grown.AddNode(model.Node{ID: storetest.ID(model.KindFunction, "app.extra"), Repo: storetest.Repo, Kind: model.KindFunction, Name: "extra"})
	for i := range 9 {
		name := "extra" + string(rune('a'+i))
		grown.AddNode(model.Node{ID: storetest.ID(model.KindFunction, "app."+name), Repo: storetest.Repo, Kind: model.KindFunction, Name: name})
	}
	err = small.ReplaceRepo(ctx, storetest.Repo, grown)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTxCapacity), err.Error())
	assert.Contains(t, err.Error(), "CODESYNAPSE_HUGEGRAPH_TX_CAPACITY")

	// Nothing was written, and the stage was released for the next attempt.
	_, err = s.Node(ctx, storetest.ID(model.KindFunction, "app.extra"))
	assert.True(t, errors.Is(err, store.ErrNotFound))
	main, err := s.Node(ctx, storetest.ID(model.KindFunction, "app.main"))
	require.NoError(t, err)
	assert.Equal(t, main.Name, "main")
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, grown))

	// One changed node is written as a delta, which fits where rewriting the
	// whole repository would not.
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	one := storetest.Fixture()
	m, _ := one.Node(storetest.ID(model.KindFunction, "app.main"))
	m.Summary = "changed"
	one.AddNode(*m)
	require.NoError(t, small.ReplaceRepo(ctx, storetest.Repo, one))
	assert.Equal(t, small.lastPublish.Load(), "delta")
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
}

func TestTxCapacityAboveServerMaximum(t *testing.T) {
	_, err := New(context.Background(), Config{URL: "http://127.0.0.1:1", TxCapacity: maxTxCapacity + 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the server maximum")
}

func TestGraphBinding(t *testing.T) {
	for core, want := range map[string]string{
		"1.5.0": "hugegraph", "1.6.2": "hugegraph", "1.7.0": "DEFAULT-hugegraph", "1.10.1": "DEFAULT-hugegraph",
		"2.0.0": "DEFAULT-hugegraph", "": "hugegraph", "garbage": "hugegraph",
	} {
		assert.Equal(t, graphBinding(core, "hugegraph"), want, core)
	}
}

// A node label created before a property existed must gain it, not be
// re-created: HugeGraph refuses ifNotExist().create() for a label whose
// properties differ, which stopped every API start after a release that added
// a node property.
func TestNodeSchemaUpgradesAnOlderLabel(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	ctx := t.Context()
	s, err := New(ctx, Config{URL: url, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))

	// A fixture label with only the identifying properties stands for the
	// node label of an older release.
	const legacy = `def s=graph.schema()
s.propertyKey('nid').asText().ifNotExist().create();s.propertyKey('repo').asText().ifNotExist().create();s.propertyKey('kind').asText().ifNotExist().create()
s.vertexLabel('legacy_node_fixture').useCustomizeStringId().properties('nid','repo','kind').ifNotExist().create();1`
	const cleanup = `graph.schema().vertexLabel('legacy_node_fixture').remove();1`
	require.NoError(t, s.write(ctx, legacy, nil, nil))
	t.Cleanup(func() { _ = s.write(context.Background(), cleanup, nil, nil) })

	upgrade := strings.Replace(schemaScript, "label('node',", "label('legacy_node_fixture',", 1)
	require.NotEqual(t, upgrade, schemaScript)
	require.NoError(t, s.write(ctx, upgrade, schemaBindings(), nil), "an older label is upgraded, not re-created")
	require.NoError(t, s.write(ctx, upgrade, schemaBindings(), nil), "and the upgrade is idempotent")

	var props []string
	require.NoError(t, s.write(ctx, `graph.schema().getVertexLabel('legacy_node_fixture').properties().collect{graph.propertyKey(it).name()}`, nil, &props))
	for _, want := range []string{"code_absent", "signature", "security_tags"} {
		assert.True(t, slices.Contains(props, want), "the fixture label gained %s: %v", want, props)
	}
}
