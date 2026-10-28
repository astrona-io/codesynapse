package mcpserver

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestCallOutcome(t *testing.T) {
	failed := &mcp.CallToolResult{IsError: true}
	for _, tc := range []struct {
		name    string
		res     mcp.Result
		err     error
		refused bool
		want    string
	}{
		{"ok", &mcp.CallToolResult{}, nil, false, "ok"},
		{"prompt ok", &mcp.GetPromptResult{}, nil, false, "ok"},
		{"tool failed", failed, nil, false, "error"},
		{"protocol error", nil, errors.New("boom"), false, "error"},
		{"refused with a result", failed, nil, true, "refused"},
		{"refused with an error", nil, errors.New("no policy"), true, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, callOutcome(tc.res, tc.err, tc.refused), tc.want)
		})
	}
}

func TestMarkRefusedOnlyWithAMarker(t *testing.T) {
	markRefused(context.Background()) // no marker: nothing to set, no panic
	refused := new(bool)
	markRefused(context.WithValue(context.Background(), refusalKey{}, refused))
	assert.True(t, *refused)
}
