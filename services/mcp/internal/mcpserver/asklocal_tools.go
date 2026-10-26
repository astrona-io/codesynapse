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
}
