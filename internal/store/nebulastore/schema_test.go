package nebulastore

import (
	"reflect"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// TestSchemaCoversNodeProps: every property nodeprops writes for a node
// with every field set has a node column of a matching type.
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
		case reflect.Float64:
			f.SetFloat(1.5)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.ValueOf([]string{"x"}))
		}
	}
	for k, val := range nodeRow(n, "run") {
		var cols []string
		switch val.(type) {
		case string:
			cols = slices.Concat(nodeTexts, nodeLists)
		case int64:
			cols = nodeLongs
		case bool:
			cols = nodeBools
		case float64:
			cols = nodeDoubles
		}
		assert.True(t, slices.Contains(cols, k), "property %q (%T) has a node column of its type", k, val)
	}
}
