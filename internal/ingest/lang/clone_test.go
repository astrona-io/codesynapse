package lang

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

const pyTotal = `def total(items, limit):
    """Sum the items under the limit."""
    result = 0
    for item in items:
        # skip big ones
        if item > limit:
            continue
        result += convert(item, "x")
    return result
`

// Renamed variables, other literals and no docstring: still the same code.
const pyRenamed = `def add_all(values, cap):
    acc = 0
    for v in values:
        if v > cap:
            continue
        acc += convert(v, 'other')
    return acc
`

const pyOtherCall = `def total(items, limit):
    result = 0
    for item in items:
        if item > limit:
            continue
        result += other(item, "x")
    return result
`

const pyDeeper = `def total(items, limit):
    result = 0
    for item in items:
        if item > limit:
            continue
    result += convert(item, "x")
    return result
`

const jsTotal = `function total(items, limit) {
  // sum
  let result = 0
  for (const item of items) {
    if (item > limit) {
      continue
    }
    result += convert(item, 'x')
  }
  return result
}
`

const jsRenamed = `function addAll(values, cap) {
  let acc = 0
  for (const v of values) {
    if (v > cap) {
      continue
    }
    acc += convert(v, "y")
  }
  return acc
}
`

func TestCloneHash(t *testing.T) {
	py := cloneHash(Python, []byte(pyTotal))
	assert.True(t, py != "", "big enough")
	assert.Equal(t, cloneHash(Python, []byte(pyRenamed)), py, "renamed variables, literals and docstring")
	assert.True(t, cloneHash(Python, []byte(pyOtherCall)) != py, "different callee")
	assert.True(t, cloneHash(Python, []byte(pyDeeper)) != py, "indentation is structure")
	assert.Equal(t, cloneHash(Python, []byte("def f(a):\n    return a\n")), "", "small")

	js := cloneHash(JavaScript, []byte(jsTotal))
	assert.True(t, js != "")
	assert.Equal(t, cloneHash(JavaScript, []byte(jsRenamed)), js, "renamed variables and literals")
	assert.Equal(t, cloneHash(TypeScript, []byte(jsTotal)), js, "language does not change the fingerprint")
	assert.True(t, js != py, "languages differ")
	assert.Equal(t, cloneHash(Python, []byte("# @generated\n"+pyTotal)), "", "generated code")
}

func TestExtractSetsCloneHash(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(s), 0o600))
	}
	write("a/calc.py", pyTotal)
	write("b/sum.py", pyRenamed)
	write("web/calc.js", jsTotal)
	write("web/Calc.vue", "<template><div/></template>\n<script setup>\n"+jsRenamed+"</script>\n")
	write("tests/test_calc.py", pyTotal)

	res, err := Extract(context.Background(), "r", root)
	require.NoError(t, err)
	byFile := map[string]string{}
	for _, n := range res.Graph.Nodes() {
		if n.Kind == model.KindFunction {
			byFile[n.File] = n.CloneHash
		}
	}
	assert.True(t, byFile["a/calc.py"] != "")
	assert.Equal(t, byFile["b/sum.py"], byFile["a/calc.py"])
	assert.True(t, byFile["web/calc.js"] != "")
	assert.Equal(t, byFile["web/Calc.vue"], byFile["web/calc.js"], "script blocks of Vue files are fingerprinted too")
	assert.Equal(t, byFile["tests/test_calc.py"], "", "tests are skipped")
}
