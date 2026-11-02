package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/asklocal"
)

// ask_local reads files of the developer's checkout, so like the git tools
// the bridge answers it; it sends the files to the API, which asks a local
// model. A direct call gets this error.
var errAskLocal = errors.New("ask_local reads files on your machine: connect through the CodeSynapse bridge (`codesynapse mcp install`, or the Claude Code plugin)")

func registerAskLocal(s *mcp.Server, add func(name, desc string) *mcp.Tool) {
	mcp.AddTool(s, add(asklocal.Tool,
		"Have a local model read files for you and answer a question about them, so their content does not enter your context: how something works, where something is handled, what a long file, directory or log in the repository contains. Pass paths (files, directories, or file:start-end ranges; only files git tracks are read, up to 2 MiB) and a question. The answer is short, with file:line citations checked against the lines it was given; read the cited lines before you edit or rely on a detail. grounded false or a note means: verify, or read the files yourself. Fails when no local model is configured; then read the files as usual. Never sends the files to a cloud model."),
		func(context.Context, *mcp.CallToolRequest, asklocal.In) (*mcp.CallToolResult, any, error) {
			return nil, nil, errAskLocal
		})
	mcp.AddTool(s, add(asklocal.DraftTool,
		"Have a local model draft self-contained, well-specified code (a helper function, a test table, boilerplate) from a task and context files, so writing it costs you fewer tokens. Pass the task (what to write, its inputs and outputs, conventions to follow), paths (context files, directories or file:start-end ranges; only files git tracks are read) and optionally target (the file the code is for; target path:start-end means the draft replaces those lines, which are sent too). The answer is a code block (the replacement for target's lines, or new code) or, if the model wrote one, a diff checked against the lines sent, plus notes with checked citations and problems found before you saw it (Go syntax errors, diff lines that do not match). The draft is unverified: compare it with the real file and apply it with your own edit, never blindly. Writes no files. Do not use it for security-sensitive code. Fails when no local model is configured; then write the code yourself. Never sends the files to a cloud model."),
		func(context.Context, *mcp.CallToolRequest, asklocal.DraftIn) (*mcp.CallToolResult, any, error) {
			return nil, nil, errLocalDraft
		})
	mcp.AddTool(s, add(asklocal.ReadTool,
		"Read the exact lines of several file:start-end ranges in one call, numbered, instead of one Read per range: the ranges minimal_context, file_outline, get_symbol_context or an ask_local citation gave you. Pass ranges (at most 50, only files git tracks; a range past the end of a file is cut to it). Returns the text of the lines, at most 256 KiB, read from your checkout as it is now. Use it for the text you are about to edit or quote; to learn what a long file says, ask_local is cheaper."),
		func(context.Context, *mcp.CallToolRequest, asklocal.ReadIn) (*mcp.CallToolResult, any, error) {
			return nil, nil, errReadRanges
		})
}

var errReadRanges = errors.New("read_ranges reads files on your machine: connect through the CodeSynapse bridge (`codesynapse mcp install`, or the Claude Code plugin)")

var errLocalDraft = errors.New("local_draft reads files on your machine: connect through the CodeSynapse bridge (`codesynapse mcp install`, or the Claude Code plugin)")
