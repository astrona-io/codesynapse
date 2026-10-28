package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/ghtools"
	"github.com/astrona-io/codesynapse/internal/gittools"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// The git tools (git_status, git_diff_summary, git_blame, git_log) read the
// developer's checkout, which the server cannot see. The bridge answers them
// here, from deterministic git commands, and then tells the server which tool
// ran, how long it took and how large the answer was, so the call is counted
// on Statistics and recorded in the audit log like any other. Nothing else
// about the checkout leaves the machine.

// isLocalTool reports whether the bridge answers tool itself: the git tools
// and the read-only gh tools (internal/ghtools, the developer's own sign-in).
func isLocalTool(tool string) bool {
	return slices.Contains(gittools.Tools, tool) || slices.Contains(ghtools.Tools, tool)
}

// localGitTimeout bounds one answer.
const localGitTimeout = 30 * time.Second

// answerGit answers a tools/call for one of the git tools and reports
// whether it did.
func (b *bridge) answerGit(ctx context.Context, msg []byte, m rpcMessage) bool {
	var c toolCall
	if json.Unmarshal(msg, &c) != nil || !isLocalTool(c.Params.Name) || len(m.ID) == 0 {
		return false
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, ghtools.Timeout(localGitTimeout, c.Params.Name, c.Params.Arguments))
	defer cancel()

	result, failed := b.runGit(ctx, c)
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err != nil {
		out = errorResponse(m.ID, err)
		failed = true
	}
	b.write(out)
	b.reportLocal(ctx, c.Params.Name, time.Since(start), len(out)/4, failed)
	return true
}

// runGit produces the tool result: the answer, or an error the agent can act on.
func (b *bridge) runGit(ctx context.Context, c toolCall) (map[string]any, bool) {
	fail := func(err error) (map[string]any, bool) {
		msg, _, _ := strings.Cut(err.Error(), "\n")
		return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": msg}}}, true
	}
	dir, err := b.checkout(ctx)
	if err != nil {
		return fail(err)
	}
	var ans any
	if slices.Contains(ghtools.Tools, c.Params.Name) {
		ans, err = ghtools.Run(ctx, dir, c.Params.Name, c.Params.Arguments)
	} else {
		ans, err = gittools.Run(ctx, dir, c.Params.Name, c.Params.Arguments)
	}
	if err != nil {
		return fail(err)
	}
	raw, err := json.Marshal(ans)
	if err != nil {
		return fail(err)
	}
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(raw)}},
		"structuredContent": json.RawMessage(raw),
		"_meta":             map[string]any{"codesynapse/source": "local-git"},
	}, false
}

var errNoCheckout = errors.New("the bridge was not started inside a git checkout, so the git tools have nothing to read")

// checkout is the top level of the repository the bridge runs in.
func (b *bridge) checkout(ctx context.Context) (string, error) {
	b.gitMu.Lock()
	defer b.gitMu.Unlock()
	if b.gitDir != "" {
		return b.gitDir, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", errNoCheckout
	}
	out, err := gitx.Run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errNoCheckout
	}
	b.gitDir = strings.TrimSpace(string(out))
	return b.gitDir, nil
}

// localUsage is what the server is told about a local call.
type localUsage struct {
	Tool           string `json:"tool"`
	Client         string `json:"client"`
	DurationMs     int    `json:"duration_ms"`
	ResponseTokens int    `json:"response_tokens"`
	Failed         bool   `json:"failed,omitempty"`
}

// reportLocal tells the server about a call it did not see. A failure is
// logged and dropped: the answer was already given, and the counts are best
// effort.
func (b *bridge) reportLocal(ctx context.Context, tool string, d time.Duration, tokens int, failed bool) {
	if b.report.base == "" {
		return
	}
	body := localUsage{Tool: tool, Client: "claude-code", DurationMs: int(d.Milliseconds()), ResponseTokens: tokens, Failed: failed}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := b.report.do(rctx, http.MethodPost, "/api/v1/usage/local", body, nil); err != nil && b.logger != nil {
		b.logger.Debug("local call not reported", "tool", tool, "error", err)
	}
}
