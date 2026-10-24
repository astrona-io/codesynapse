package gittools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// The tool names, in the order they are listed. They read a checkout, so only
// the bridge on the developer's machine can answer them.
const (
	ToolStatus      = "git_status"
	ToolDiffSummary = "git_diff_summary"
	ToolBlame       = "git_blame"
	ToolLog         = "git_log"
)

// Tools lists them.
var Tools = []string{ToolStatus, ToolDiffSummary, ToolBlame, ToolLog}

// StatusIn is git_status's input: none.
type StatusIn struct{}

// Run answers one of the tools for the checkout at dir. args is the tool's
// JSON arguments (unknown fields are refused); the result is the answer to
// encode.
func Run(ctx context.Context, dir, tool string, args json.RawMessage) (any, error) {
	decode := func(v any) error {
		if len(bytes.TrimSpace(args)) == 0 {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return nil
	}
	switch tool {
	case ToolStatus:
		var in StatusIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return Status(ctx, dir)
	case ToolDiffSummary:
		var in DiffIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return DiffSummary(ctx, dir, in)
	case ToolBlame:
		var in BlameIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return Blame(ctx, dir, in)
	case ToolLog:
		var in LogIn
		if err := decode(&in); err != nil {
			return nil, err
		}
		return Log(ctx, dir, in)
	}
	return nil, fmt.Errorf("%w: unknown tool %q", ErrInvalid, tool)
}
