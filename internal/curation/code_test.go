package curation

import (
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestPrepareCodeValidation(t *testing.T) {
	id := func(c string) string { return "sym-" + strings.Repeat(c, 32) }
	for _, tt := range []struct {
		name  string
		in    CodeInput
		valid bool
	}{
		{"annotation", CodeInput{Method: "annotation", Destinations: []string{id("a")}, Purpose: "serves reports"}, true},
		{"annotation has no source repository", CodeInput{Method: "annotation", OriginRepo: "source", Destinations: []string{id("a")}, Purpose: "reports"}, false},
		{"malformed permanent ID", CodeInput{Method: "annotation", Destinations: []string{id("z")}, Purpose: "reports"}, false},
		{"invalid origin repository", CodeInput{Method: "copy", OriginRepo: "source\x00", Origins: []string{id("a")}, Destinations: []string{id("b")}}, false},
		{"unknown method", CodeInput{Method: "approved"}, false},
		{"empty", CodeInput{Method: "annotation", Destinations: []string{id("a")}}, false},
		{"self lineage", CodeInput{Method: "copy", Origins: []string{id("a")}, Destinations: []string{id("a")}}, false},
		{"split shape", CodeInput{Method: "split", Origins: []string{id("a")}, Destinations: []string{id("b")}}, false},
		{"merge shape", CodeInput{Method: "merge", Origins: []string{id("a")}, Destinations: []string{id("b")}}, false},
		{"bounded", CodeInput{Method: "annotation", Destinations: []string{id("a")}, Purpose: strings.Repeat("x", 1001)}, false},
		{"missing assessment", CodeInput{Method: "copy", Origins: []string{id("a")}, Destinations: []string{id("b")}, Reason: "fits"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := PrepareCode("r", "o", "u", "codex", tt.in, time.Now())
			assert.Equal(t, err == nil, tt.valid)
		})
	}
}
