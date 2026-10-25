package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/ghtools"
	"github.com/astrona-io/codesynapse/internal/gittools"
)

// The git tools read the developer's checkout, which only the machine it is
// on can see. The server registers them so every client lists the same tools,
// and the Claude Code bridge answers them locally and tells the server (tool,
// size, time; nothing else) so they are counted and audited. A client that
// calls the server directly gets this error instead of a guess.
var errGitLocal = errors.New("the git tools read the checkout on your machine: connect through the CodeSynapse bridge (`codesynapse mcp install`, or the Claude Code plugin); the server cannot see your working tree")

func registerGitTools(s *mcp.Server, add func(name, desc string) *mcp.Tool) {
	local := func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return nil, nil, errGitLocal
	}
	mcp.AddTool(s, add(gittools.ToolStatus,
		"git status and git diff --stat in one small answer: branch, head, ahead/behind its upstream, and every changed path (staged, unstaged, untracked) with added/removed lines. Use instead of running git status or git diff --stat."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ gittools.StatusIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
	mcp.AddTool(s, add(gittools.ToolDiffSummary,
		"What changed and where, without the patch: per changed file its status, added/removed lines and hunk line ranges with the declaration git names for each (untracked files included). Default is everything uncommitted since HEAD; base compares with the point where the branch left it. Read only those ranges, or pass them on to get_pr_context for symbols, risk and tests. Use instead of git diff."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ gittools.DiffIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
	mcp.AddTool(s, add(gittools.ToolBlame,
		"Who last changed lines start..end of a file: one entry per run of lines from the same commit (commit, author, date, subject), uncommitted lines marked. At most 400 lines. Use instead of git blame."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ gittools.BlameIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
	mcp.AddTool(s, add(gittools.ToolLog,
		"Recent commits for the repository or a path, one compact entry each: commit, date, author, subject, files and lines changed, the files touched most. Use instead of git log."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ gittools.LogIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
	// The gh tools are read-only and answered on the developer's machine with
	// their own GitHub sign-in, like the git tools. There is no tool that
	// creates, merges or closes anything.
	mcp.AddTool(s, add(ghtools.ToolPRStatus,
		"Where a pull request stands, in a few lines: state, draft, head and base branch, mergeability, review decision, and its checks counted (pass, fail, pending, skipped, done) with only the failing and waiting ones named. Omit number for the checked-out branch's pull request. Set wait_seconds (at most 300) to block until the checks finish or one fails, instead of polling: the answer then says waited_seconds and timed_out. Use instead of gh pr view, gh pr checks or gh pr checks --watch. Reads only; creating and merging stay with gh."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ ghtools.PRStatusIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
	mcp.AddTool(s, add(ghtools.ToolPRList,
		"Pull requests of the checkout's GitHub repository, one line each: number, title, head branch, state, draft. state is open (default), closed, merged or all; at most 30. Use instead of gh pr list."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ ghtools.PRListIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
	mcp.AddTool(s, add(ghtools.ToolRunFail,
		"Why a CI run failed, in a few lines: the failed jobs, their failed steps and the error lines of their logs (test failures, panics, compiler and linter messages), without timestamps or the rest of the log. Pass run for a workflow run ID, or omit it for the latest failed run of the pull request's branch (number, or the checked-out branch). Use instead of gh run view --log-failed. Reads only."),
		func(ctx context.Context, r *mcp.CallToolRequest, _ ghtools.RunFailureIn) (*mcp.CallToolResult, any, error) {
			return local(ctx, r, nil)
		})
}
