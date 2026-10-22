package hugegraphstore

import (
	"context"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/internal/vector/milvus"
)

// TestContract runs against a real HugeGraph when CODESYNAPSE_TEST_HUGEGRAPH_URL
// is set (just test-hugegraph). It deletes the fixture repository and the
// memory of its namespaces, so never point it at a graph you care about.
func TestContract(t *testing.T) {
	url := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_HUGEGRAPH_URL not set")
	}
	ctx := context.Background()
	var vec vector.Index = vector.NewFlat()
	if mu := os.Getenv("CODESYNAPSE_TEST_MILVUS_URL"); mu != "" {
		mx, err := milvus.New(ctx, milvus.Config{URL: mu, Prefix: "cs_stest_"})
		require.NoError(t, err)
		require.NoError(t, mx.Drop(ctx))
		t.Cleanup(func() { _ = mx.Drop(ctx) })
		vec = mx
	}
	s, err := New(ctx, Config{URL: url, Vectors: vec, User: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_USER"), Password: os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close(ctx) })
	require.NoError(t, s.EnsureSchema(ctx))
	require.NoError(t, s.EnsureSchema(ctx), "the schema is idempotent")
	require.NoError(t, s.DeleteRepo(ctx, storetest.Repo))
	storetest.Run(t, s)
}

// TestSchemaCoversNodeProps fills every node field and checks each property
// nodeprops writes has a declared type here.
func TestSchemaCoversNodeProps(t *testing.T) {
	var n model.Node
	v := reflect.ValueOf(&n).Elem()
	for i := range v.NumField() {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int:
			f.SetInt(1)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.ValueOf([]string{"x"}))
		}
	}
	declared := slices.Concat(nodeTexts, nodeLongs, nodeBools, nodeLists)
	for k := range nodeprops.To(n) {
		if k == "id" {
			k = "nid"
		}
		assert.True(t, slices.Contains(declared, k), "node property %q has no HugeGraph type", k)
	}
}
