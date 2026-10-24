// Package agentrules is the short ruleset every agent session starts with, so
// CodeSynapse is used before files are read without anyone pasting a prompt.
// The API serves it (GET /api/v1/agent-rules) and the plugin's SessionStart
// hook puts it in the session's context. An operator can replace the text
// without a release; the built-in one is the fallback when the server is
// unreachable.
package agentrules

import (
	"io"
	"os"
	"strings"
)

// MaxBytes bounds a custom ruleset: it is added to every session's context.
const MaxBytes = 4 << 10

// Version changes when the built-in text changes meaning, so the Statistics
// page can tell which wording a session ran with.
const Version = 1

// Default is the built-in ruleset.
const Default = `CodeSynapse is connected for this repository. Use its tools before you read or search files:
1. Start every task with minimal_context (the task as the question); read only the file ranges it returns.
2. Before editing a symbol, call get_symbol_context (outline first, then full). Use get_implementations for interfaces and get_call_graph to trace callers; do not grep for them.
3. Call file_outline before opening a large file, and read only the ranges you need.
4. Before a rename, a signature change or a removal, call change_checklist and work through every item.
5. Before you finish, call verify_plan and run the commands it lists, then get_pr_context on your diff and fix what it flags as untested, high risk or security-sensitive.
Use Read and Grep for what the graph does not answer. The graph reflects the last ingest, so files you edited in this session may differ.`

// Load returns the ruleset: the contents of file when it is set and usable
// (non-empty, at most MaxBytes), otherwise the built-in text. custom says
// which one it is.
func Load(file string) (text string, custom bool) {
	if file == "" {
		return Default, false
	}
	f, err := os.Open(file) //nolint:gosec // the operator's own file, named in the environment
	if err != nil {
		return Default, false
	}
	defer f.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil || len(b) > MaxBytes {
		return Default, false
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, true
	}
	return Default, false
}
