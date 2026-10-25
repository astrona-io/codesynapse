package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/toolsets"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// Retrieval usefulness (docs/PLAN.md, agent workflow roadmap step 13): did
// the agent read what the tools pointed it to? The hooks keep the ranges
// in the session's state file on this machine; only per-tool counts are
// reported.

const (
	maxTrackedRanges = 500
	// maxRangesPerAnswer bounds what one answer can add.
	maxRangesPerAnswer = 100
	retrievalClient    = "claude-code"
)

// rangeRe finds "path/file.ext:12" and "path/file.ext:12-30" in answers.
var rangeRe = regexp.MustCompile(`([A-Za-z0-9_.@+-]+(?:/[A-Za-z0-9_.@+-]+)*\.[A-Za-z0-9]{1,8}):(\d{1,7})(?:-(\d{1,7}))?`)

// toolOf is the CodeSynapse tool a hook's tool name calls
// ("mcp__codesynapse__search", "mcp__plugin_codesynapse_codesynapse__search"),
// or "" for another server's.
func toolOf(name string) string {
	if !strings.HasPrefix(name, "mcp__") || !strings.Contains(name, "codesynapse") {
		return ""
	}
	i := strings.LastIndex(name, "__")
	if tool := name[i+2:]; toolsets.Known(tool) {
		return tool
	}
	return ""
}

// noteAnswer records the ranges a CodeSynapse answer gave.
func noteAnswer(in toolHookInput) {
	tool := toolOf(in.ToolName)
	state := stateFile(in.SessionID)
	if tool == "" || state == "" || len(in.ToolResponse) == 0 {
		return
	}
	var texts []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			texts = append(texts, v)
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	var resp any
	if json.Unmarshal(in.ToolResponse, &resp) != nil {
		return
	}
	walk(resp)
	s := loadState(state)
	seen := map[answerRange]bool{}
	for _, r := range s.Ranges {
		seen[answerRange{File: r.File, Start: r.Start, End: r.End}] = true
	}
	added := 0
	for _, text := range texts {
		for _, m := range rangeRe.FindAllStringSubmatch(text, -1) {
			if added == maxRangesPerAnswer {
				break
			}
			start, _ := strconv.Atoi(m[2]) //nolint:errcheck // the pattern only matches digits
			end := start
			if m[3] != "" {
				end, _ = strconv.Atoi(m[3]) //nolint:errcheck // the pattern only matches digits
			}
			key := answerRange{File: m[1], Start: start, End: end}
			if start < 1 || end < start || strings.Contains(text, "://"+m[1]) || seen[key] {
				continue
			}
			seen[key] = true
			added++
			s.Ranges = append(s.Ranges, answerRange{Tool: tool, Repo: in.ToolInput.Repo, File: m[1], Start: start, End: end})
			s.Counts = bump(s.Counts, tool, 1, 0)
		}
	}
	if added > 0 {
		saveState(state, s)
	}
}

// noteRead marks the answered ranges a Read covered as read.
func noteRead(in toolHookInput) {
	state := stateFile(in.SessionID)
	file := filepath.ToSlash(in.ToolInput.FilePath)
	if state == "" || file == "" {
		return
	}
	s := loadState(state)
	from, to := 1, int(^uint(0)>>1)
	if in.ToolInput.Offset > 0 {
		from = in.ToolInput.Offset
	}
	if in.ToolInput.Limit > 0 {
		to = from + in.ToolInput.Limit - 1
	}
	changed := false
	for i := range s.Ranges {
		r := &s.Ranges[i]
		sameFile := file == r.File || strings.HasSuffix(file, "/"+r.File)
		if r.Read || !sameFile || r.End < from || r.Start > to {
			continue
		}
		r.Read, changed = true, true
		s.Counts = bump(s.Counts, r.Tool, 0, 1)
	}
	if changed {
		saveState(state, s)
	}
}

func bump(m map[string]rangeCounts, tool string, returned, read int) map[string]rangeCounts {
	if m == nil {
		m = map[string]rangeCounts{}
	}
	c := m[tool]
	c.Returned += returned
	c.Read += read
	m[tool] = c
	return m
}

// reportRetrieval sends what was counted since the last report. Ranges of
// calls that named another repository are not this checkout's: a session
// reports for the repository it runs in. Failures are logged and retried
// at the next Stop.
func reportRetrieval(ctx context.Context, in toolHookInput, logger *slog.Logger) {
	state := stateFile(in.SessionID)
	if state == "" || in.Cwd == "" {
		return
	}
	s := loadState(state)
	total, tokens := hookTokenDelta(in.TranscriptPath, s.TokenSent)
	hasEdits := s.Edits.Edits+s.Edits.Writes > 0
	w := newWorktree(ctx, in.Cwd, "", logger)
	if w == nil {
		return
	}
	wf := s.workflowReport()
	// Edits: what grew since the last report (a reverted file is not taken back).
	edited := sessionEdits(ctx, w.root, s.Base)
	nEdited, nFound := len(edited), found(edited, s.Ranges, w.repo)
	for key, d := range map[string]int{"files_edited": nEdited - s.Reported.Edited, "files_edited_found": nFound - s.Reported.Found} {
		if d > 0 {
			if wf == nil {
				wf = map[string]int{}
			}
			wf[key] = d
		}
	}
	bash := s.bashReport()
	if len(s.Counts) == 0 && wf == nil && tokens == nil && !hasEdits && bash == nil {
		return
	}
	// Counts of other repositories' ranges are left out of the report.
	foreign := map[string]rangeCounts{}
	for _, r := range s.Ranges {
		if r.Repo != "" && r.Repo != w.repo {
			read := 0
			if r.Read {
				read = 1
			}
			foreign = bump(foreign, r.Tool, 1, read)
		}
	}
	delta := map[string]rangeCounts{}
	for tool, c := range s.Counts {
		sent, other := s.Sent[tool], foreign[tool]
		d := rangeCounts{Returned: c.Returned - sent.Returned - other.Returned, Read: c.Read - sent.Read - other.Read}
		if d.Returned > 0 || d.Read > 0 {
			delta[tool] = rangeCounts{Returned: max(d.Returned, 0), Read: max(d.Read, 0)}
		}
	}
	if len(delta) == 0 && wf == nil && tokens == nil && !hasEdits && bash == nil {
		return
	}
	c := newAPIClient(app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080"))
	c.http.Timeout = 5 * time.Second
	client := in.Client
	if client == "" {
		client = retrievalClient
	}
	body := map[string]any{"client": client, "tools": delta}
	if hasEdits {
		body["edits"] = s.Edits
	}
	if tokens != nil {
		body["tokens"] = tokens
	}
	if wf != nil {
		body["workflow"] = wf
	}
	if bash != nil {
		body["bash"] = bash
	}
	err := w.call(ctx, c, func(repo string) error {
		return c.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/usage/retrieval", body, nil)
	})
	if err != nil {
		logger.Debug("retrieval counts not reported", "error", err)
		return
	}
	// Everything counted so far is accounted for, sent or foreign.
	s.Sent = map[string]rangeCounts{}
	for tool, c := range s.Counts {
		s.Sent[tool] = rangeCounts{Returned: c.Returned - foreign[tool].Returned, Read: c.Read - foreign[tool].Read}
	}
	s.markReported()
	s.BashSent = maps.Clone(s.Bash)
	s.Reported.Edited, s.Reported.Found = max(s.Reported.Edited, nEdited), max(s.Reported.Found, nFound)
	if hasEdits {
		s.Edits = usage.EditCounts{}
	}
	if tokens != nil {
		s.TokenSent = total
	}
	saveState(state, s)
}
