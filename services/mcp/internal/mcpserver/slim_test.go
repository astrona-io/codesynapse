package mcpserver

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestSlimJSON(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"empties dropped":   {`{"a":null,"b":"","c":[],"d":{},"e":"x","n":{"z":null}}`, `{"e":"x"}`},
		"zero and false":    {`{"a":0,"b":false,"c":[null,1]}`, `{"a":0,"b":false,"c":[null,1]}`},
		"big number exact":  {`{"id":12345678901234567890,"x":""}`, `{"id":12345678901234567890}`},
		"plain text":        {`no json here`, `no json here`},
		"invalid":           {`{"a":`, `{"a":`},
		"nothing to remove": {`{"a":1}`, `{"a":1}`},
	} {
		t.Run(name, func(t *testing.T) { assert.Equal(t, slimJSON(tc.in), tc.want) })
	}
}

func FuzzSlimJSON(f *testing.F) {
	f.Add(`{"a":null,"b":[1,{"c":""}]}`)
	f.Fuzz(func(t *testing.T, s string) {
		if got := slimJSON(s); len(got) > len(s) {
			t.Fatalf("grew: %q -> %q", s, got)
		}
	})
}
