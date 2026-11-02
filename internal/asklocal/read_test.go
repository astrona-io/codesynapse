package asklocal

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestReadInValidate(t *testing.T) {
	assert.NoError(t, ReadIn{Ranges: []string{"a.go:1-2", "b/c.go:10-40"}}.Validate())
	many := make([]string, MaxPaths+1)
	for i := range many {
		many[i] = "a.go:1-2"
	}
	for name, in := range map[string]ReadIn{
		"empty":      {},
		"whole file": {Ranges: []string{"a.go:1-2", "b.go"}},
		"reversed":   {Ranges: []string{"a.go:5-2"}},
		"zero":       {Ranges: []string{"a.go:0-2"}},
		"too many":   {Ranges: many},
	} {
		assert.ErrorIs(t, in.Validate(), ErrInvalid, name)
	}
}

func TestRenderRanges(t *testing.T) {
	got := RenderRanges([]File{
		{Path: "a.go", Start: 2, End: 3, Text: "x\ny"},
		{Path: "b.go", Start: 1, End: 1, Text: "z\n"},
	}, []string{"c.bin"})
	assert.Equal(t, got, "=== a.go (lines 2-3) ===\n2| x\n3| y\n\n=== b.go (lines 1-1) ===\n1| z\n\nNot read (binary, too large or a symlink): c.bin\n")
	assert.Equal(t, RenderRanges(nil, nil), "")
}
