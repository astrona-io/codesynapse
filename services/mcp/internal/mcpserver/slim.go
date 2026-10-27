package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// slimMiddleware removes empty values (null, "", [] and {}) from the JSON text
// of tool results, so agents do not pay for fields that say nothing. Zero
// numbers and false stay: they carry meaning. Text that is not a JSON object
// or array, and rewrites that are not shorter, are left alone. The structured
// copy is untouched because clients may validate it against the output schema.
func slimMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if r, ok := res.(*mcp.CallToolResult); ok && r != nil && !r.IsError {
				for _, c := range r.Content {
					if t, ok := c.(*mcp.TextContent); ok {
						t.Text = slimJSON(t.Text)
					}
				}
			}
			return res, err
		}
	}
}

func slimJSON(text string) string {
	trimmed := bytes.TrimSpace([]byte(text))
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return text
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber() // numbers keep their exact digits
	var v any
	if dec.Decode(&v) != nil || dec.More() {
		return text
	}
	out, err := json.Marshal(prune(v))
	if err != nil || len(out) >= len(trimmed) {
		return text
	}
	return string(out)
}

// prune drops empty values below the root; the root itself is always kept.
func prune(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			e = prune(e)
			if isEmpty(e) {
				delete(x, k)
				continue
			}
			x[k] = e
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = prune(e)
		}
		return x
	}
	return v
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}
