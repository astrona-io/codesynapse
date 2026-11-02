package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/asklocal"
	"github.com/astrona-io/codesynapse/internal/execution"
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
// overrides it. The plugin gives the hook more. briefPlanBudget bounds
// finding the repository and the brief-plan answer each: a server that
// takes longer to plan will not write a brief in time either.
const (
	prebriefTimeout = 30 * time.Second
	briefPlanBudget = 3 * time.Second
)

// errBriefSlow means the server did not plan or write the brief in time.
var errBriefSlow = errors.New("the brief took too long")

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
	switch {
	case chatOffline(in.SessionID) || serverDown(o.apiURL):
		noteBrief(briefOffline, 0)
		return nil
	case briefSkipped(in.SessionID):
		noteBrief(briefSkippedChat, 0)
		return nil
	}
	limit := prebriefTimeout
	if n, err := strconv.Atoi(os.Getenv("CODESYNAPSE_PREBRIEF_SECONDS")); err == nil && n > 0 {
		limit = time.Duration(n) * time.Second
	}
	started := time.Now()
	bctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	text, err := prebrief(bctx, newHookClient(o.apiURL), in)
	took := time.Since(started)
	switch {
	case errors.Is(err, errServerOffline) || connectFailure(err) != "":
		noteBrief(briefOffline, took)
	case ctx.Err() == nil && (errors.Is(err, errBriefSlow) || errors.Is(err, context.DeadlineExceeded) || bctx.Err() != nil):
		// Slow, not gone: the next prompts of this chat would wait as long.
		skipBriefs(in.SessionID)
		noteBrief(briefTimeout, took)
	case err != nil:
		noteBrief(briefFailed, took)
	case text == "":
		noteBrief(briefNothing, took)
	default:
		noteBrief(briefWritten, took)
	}
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
	ctx, err = c.registerWithin(ctx, in.SessionID, in.SessionID, hookRegisterBudget)
	if err != nil {
		return "", err
	}
	var repo string
	if err := within(ctx, briefPlanBudget, func(ctx context.Context) (err error) {
		repo, err = c.resolveRepo(ctx, root)
		return err
	}); err != nil {
		return "", err
	}
	api := c
	// The context bounds it; the hook client gives up after seconds. The
	// transport keeps the short connect budget.
	api.http = &http.Client{Transport: c.http.Transport}
	var plan struct {
		Ranges []string `json:"ranges"`
	}
	if err := within(ctx, briefPlanBudget, func(ctx context.Context) error {
		return api.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/brief-plan", map[string]string{"prompt": in.Prompt}, &plan)
	}); err != nil {
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

// within runs f with ctx bounded by d, and says errBriefSlow when d ran out
// before the brief's own limit did.
func within(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := f(wctx)
	if err != nil && wctx.Err() != nil && ctx.Err() == nil && connectFailure(err) == "" {
		return fmt.Errorf("%w: %w", errBriefSlow, err)
	}
	return err
}

// The outcomes noteBrief records for doctor.
const (
	briefWritten     = "briefed"
	briefNothing     = "nothing"
	briefOffline     = "offline"
	briefTimeout     = "timeout"
	briefSkippedChat = "skipped-chat"
	briefFailed      = "failed"
)

// briefOutcomes is the text doctor shows for each outcome.
var briefOutcomes = map[string]string{
	briefWritten:     "a brief was added",
	briefNothing:     "nothing worth a brief",
	briefOffline:     "skipped: the server could not be reached",
	briefTimeout:     "timed out; skipped for the rest of that chat",
	briefSkippedChat: "skipped: a brief in that chat timed out earlier",
	briefFailed:      "failed: the server refused or answered an error",
}

// briefSkipFresh is how long a chat's skip marker counts, so it cannot
// outlive the chat for good.
const briefSkipFresh = 24 * time.Hour

// briefNote is the last pre-brief on this machine: when, how long and what
// came of it. No session, prompt, path or URL.
type briefNote struct {
	At      time.Time `json:"at"`
	Millis  int64     `json:"ms"`
	Outcome string    `json:"outcome"`
}

func (n briefNote) age(now time.Time) time.Duration { return now.Sub(n.At) }

func parseBriefNote(data []byte) (briefNote, bool) {
	if len(data) == 0 || len(data) > maxMarkBytes {
		return briefNote{}, false
	}
	var n briefNote
	if json.Unmarshal(data, &n) != nil || n.At.IsZero() || n.Millis < 0 {
		return briefNote{}, false
	}
	if _, ok := briefOutcomes[n.Outcome]; !ok {
		return briefNote{}, false
	}
	return n, true
}

func briefNoteFile() string {
	if dir := offlineDir(); dir != "" {
		return filepath.Join(dir, "prebrief-last.json")
	}
	return ""
}

// noteBrief records the outcome of this pre-brief for doctor.
func noteBrief(outcome string, took time.Duration) {
	file := briefNoteFile()
	if file == "" {
		return
	}
	if data, err := json.Marshal(briefNote{At: time.Now().UTC(), Millis: took.Milliseconds(), Outcome: outcome}); err == nil {
		writePrivate(file, data)
	}
}

// lastBrief is the last pre-brief recorded on this machine.
func lastBrief() (briefNote, bool) {
	return parseBriefNote(readSmall(briefNoteFile()))
}

// briefSkipFile is the chat's marker that one of its briefs timed out.
func briefSkipFile(session string) string {
	if execution.Validate(execution.Identity{TenantID: "t", SubjectID: "s", SessionID: session, ParentSessionID: session}) != nil {
		return ""
	}
	if dir := offlineDir(); dir != "" {
		return filepath.Join(dir, "brief-"+session)
	}
	return ""
}

// skipBriefs marks the chat so its later prompts are not briefed.
func skipBriefs(session string) {
	file := briefSkipFile(session)
	if file == "" {
		return
	}
	if data, err := json.Marshal(offlineMarker{At: time.Now().UTC()}); err == nil {
		writePrivate(file, data)
	}
}

// briefSkipped reports whether a brief of the chat timed out earlier.
func briefSkipped(session string) bool {
	data := readSmall(briefSkipFile(session))
	var m offlineMarker
	if data == nil || json.Unmarshal(data, &m) != nil || m.At.IsZero() {
		return false
	}
	age := time.Since(m.At)
	return age < briefSkipFresh && age > -time.Minute
}
