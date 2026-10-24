package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/taskeval"
)

// Eval cases from sessions (docs/PLAN.md, agent workflow roadmap step 17).
// When a person opts in with CODESYNAPSE_EVAL_CAPTURE=true, the hooks keep
// what they asked (the session's first prompt, redacted and shortened) and
// the files the session then changed, as a case in the checkout's git
// directory. Nothing of it is sent anywhere; `codesynapse eval --sessions`
// scores the tools on the cases.

const (
	evalCaptureEnv = "CODESYNAPSE_EVAL_CAPTURE"
	casesFileName  = "codesynapse-eval-cases.jsonl"
	maxTaskRunes   = 500
)

func evalCapture() bool { return os.Getenv(evalCaptureEnv) == "true" }

// notePrompt keeps the session's first prompt as its task.
func notePrompt(in toolHookInput) {
	state := stateFile(in.SessionID)
	if !evalCapture() || state == "" || strings.TrimSpace(in.Prompt) == "" {
		return
	}
	s := loadState(state)
	if s.Task != "" {
		return
	}
	s.Task = shortTask(redact.Free(in.Prompt))
	saveState(state, s)
}

func shortTask(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if utf8.RuneCountInString(t) > maxTaskRunes {
		t = string([]rune(t)[:maxTaskRunes])
	}
	return t
}

// casesFile is where a checkout's cases live: in its git directory, so they
// are never committed and every worktree shares them.
func casesFile(ctx context.Context, root string) string {
	out, err := gitx.Run(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	return filepath.Join(dir, casesFileName)
}

// captureCase saves the session as a case once it changed files that
// existed where it began. Later stops update the same case.
func captureCase(ctx context.Context, in toolHookInput, logger *slog.Logger) {
	state := stateFile(in.SessionID)
	if !evalCapture() || state == "" || in.Cwd == "" {
		return
	}
	s := loadState(state)
	if s.Task == "" || s.Base == nil || gitx.ValidateRev(s.Base.Head) != nil {
		return
	}
	w := newWorktree(ctx, in.Cwd, "", logger)
	if w == nil {
		return
	}
	c := taskeval.Case{Task: s.Task, Base: s.Base.Head, At: time.Now().UTC()}
	for _, p := range sessionEdits(ctx, w.root, s.Base) {
		// The answer can only name files the graph at the base has.
		if _, err := gitx.Run(ctx, w.root, "cat-file", "-e", s.Base.Head+":"+p); err == nil {
			c.Truth = append(c.Truth, p)
		} else {
			c.Added++
		}
	}
	if len(c.Truth) == 0 {
		return
	}
	sum := sha256.Sum256([]byte(in.SessionID))
	c.Session = hex.EncodeToString(sum[:8])
	path := casesFile(ctx, w.root)
	if path == "" {
		return
	}
	if err := taskeval.SaveCase(path, c); err != nil {
		logger.Debug("eval case not saved", "error", err)
	}
}
