package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestRetrievalBashValidate(t *testing.T) {
	ok := retrievalReport{Client: "claude-code", Bash: map[string]int{"git_status": 3}}
	assert.NoError(t, ok.validate())
	for name, bash := range map[string]map[string]int{
		"unknown class": {"rm_rf": 1},
		"negative":      {"git_status": -1},
		"too large":     {"git_status": maxRetrievalRanges + 1},
	} {
		t.Run(name, func(t *testing.T) {
			bad := retrievalReport{Client: "claude-code", Bash: bash}
			assert.True(t, bad.validate() != nil, "must be refused")
		})
	}
}
