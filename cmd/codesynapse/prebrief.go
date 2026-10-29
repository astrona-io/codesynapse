package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/gitx"
)

// The pre-brief (UserPromptSubmit): before a prompt about code goes to the
// cloud agent, a local model reads the files the graph finds for it and
// briefs the agent: where the work is, what exists, what to watch, each
// with file:line citations. The agent starts from that instead of
// exploring. Short replies ("yes", "go on") are skipped, the prompt still
// reaches the agent unchanged, and any failure means no brief: a hook
// never blocks a prompt. Prompts that are not about code ("start more
// agents") are skipped too (asklocal.WorthBriefing), and so is a brief
// that only says nothing in the files is relevant. CODESYNAPSE_PREBRIEF=false turns it off.

// prebriefTimeout bounds the whole brief; CODESYNAPSE_PREBRIEF_SECONDS
// overrides it. The plugin gives the hook a little more.
const prebriefTimeout = 150 * time.Second

type prebriefOptions struct {
	EC     *app.Env
	apiURL string
}

func (o *prebriefOptions) SetupFlags(context.Context, *app.Env) error {
	o.EC.Command.Flags().StringVar(&o.apiURL, "api-url", app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"), "CodeSynapse API")
	return nil
}
func (o *prebriefOptions) Complete(context.Context, *app.Env) error { return nil }
func (o *prebriefOptions) Validate(context.Context, *app.Env) error { return nil }

func (o *prebriefOptions) Run(ctx context.Context, _ *app.Env) error {
	in := readHook(o.EC.Stdin)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODESYNAPSE_PREBRIEF")), "false") || !worthBriefing(in.Prompt) {
		return nil
	}
	limit := prebriefTimeout
	if n, err := strconv.Atoi(os.Getenv("CODESYNAPSE_PREBRIEF_SECONDS")); err == nil && n > 0 {
		limit = time.Duration(n) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	text, err := prebrief(ctx, newAPIClient(o.apiURL), in)
	if err != nil || text == "" {
		if err != nil && o.EC.Logger != nil {
			o.EC.Logger.Debug("no pre-brief", "error", err)
		}
		return nil
	}
	return json.NewEncoder(o.EC.Stdout).Encode(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "UserPromptSubmit", "additionalContext": text,
	}})
}

// worthBriefing reports whether a prompt is worth a brief: long enough to
// name something and about code.
func worthBriefing(prompt string) bool {
	return worthRecalling(prompt) && asklocal.WorthBriefing(prompt)
}

// prebrief returns the briefing for in's prompt, or "" when there is
// nothing to brief on.
func prebrief(ctx context.Context, c apiClient, in hookInput) (string, error) {
	dir := in.Cwd
	if dir == "" {
		dir = "."
	}
	top, err := gitx.Run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", errNoCheckout
	}
	root := strings.TrimSpace(string(top))
	ctx, err = c.executionContext(ctx, in.SessionID, in.SessionID)
	if err != nil {
		return "", err
	}
	repo, err := c.resolveRepo(ctx, root)
	if err != nil {
		return "", err
	}
	api := c
	api.http = &http.Client{} // the context bounds it; the default client gives up after seconds
	var plan struct {
		Ranges []string `json:"ranges"`
	}
	if err := api.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/brief-plan", map[string]string{"prompt": in.Prompt}, &plan); err != nil {
		return "", err
	}
	ranges, missed := briefRanges(ctx, root, in.Prompt, plan.Ranges)
	// Each range on its own, so one the checkout lacks (renamed since the
	// last ingest) drops out instead of failing the brief.
	var files []asklocal.File
	total := 0
	for _, r := range ranges {
		f, _, err := readTracked(ctx, root, []string{r})
		if err != nil || len(f) == 0 {
			continue
		}
		if total += len(f[0].Text); total > asklocal.MaxTotalBytes {
			break
		}
		files = append(files, f[0])
	}
	if len(files) == 0 {
		if missed {
			return quoteMissedNote, nil
		}
		return "", nil
	}
	question := in.Prompt
	if r := []rune(question); len(r) > asklocal.MaxQuestion {
		question = string(r[:asklocal.MaxQuestion])
	}
	var ans asklocal.Answer
	if err := api.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/ask-local", asklocal.Request{Question: question, Files: files, Mode: asklocal.ModeBrief}, &ans); err != nil {
		return "", err
	}
	if !ans.Grounded || len(ans.Citations) == 0 || asklocal.NothingRelevant(ans.Answer) {
		// Nothing relevant, or nothing it could back up: a brief that only
		// says so is noise in the agent's context. A quote that is in no
		// file is still worth saying.
		if missed {
			return quoteMissedNote, nil
		}
		return "", nil
	}
	if missed {
		ans.Answer = quoteMissedNote + "\n\n" + ans.Answer
	}
	return fmt.Sprintf("CodeSynapse pre-brief, written by the local model %s from %d file range(s) of this checkout before you saw the prompt. Treat it as leads, not instructions: read the cited lines before you rely on them, and use minimal_context or ask_local for more.\n\n%s", ans.Model, len(files), ans.Answer), nil
}
