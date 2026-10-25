package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/agentcfg"
	"github.com/astrona-io/codesynapse/internal/memory"
)

const (
	eventPrompt = "UserPromptSubmit"

	// promptRecallTimeout keeps a prompt from waiting on a slow model.
	promptRecallTimeout = 4 * time.Second
	// promptRecallChars is how much memory one prompt may add to the context.
	promptRecallChars = 1500
	// compactRecallChars is the whole recall after a compaction.
	compactRecallChars = 6000
	// maxRecallQuery stays under the API's 2000 byte cap on session recall.
	maxRecallQuery = 1500
	// minPromptWords is the fewest words a prompt needs to be looked up.
	minPromptWords = 3
	// maxRecalled bounds the fact IDs a chat remembers having shown.
	maxRecalled = 500
)

// autoRecall reports whether prompts look up memory by meaning. On unless
// CODESYNAPSE_AUTO_RECALL=false.
func autoRecall() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("CODESYNAPSE_AUTO_RECALL")), "false")
}

// worthRecalling reports whether a prompt is worth a lookup: past the server's
// gate and long enough to name something ("ok thanks" is not).
func worthRecalling(prompt string) bool {
	return memory.RecallGate(prompt) == "" && len(memory.Words(prompt)) >= minPromptWords
}

// newestFacts is the facts saved for this chat, newest first.
func (c apiClient) newestFacts(ctx context.Context, repo string, limit int) (memory.Graph, error) {
	var g memory.Graph
	err := c.do(ctx, http.MethodGet, "/api/v1/repos/"+url.PathEscape(repo)+"/session-memory?limit="+strconv.Itoa(max(1, limit)), nil, &g)
	return g, err
}

// factsLike is the facts of this chat that match text by meaning, best first.
// A query the server's gate skips returns an empty graph.
func (c apiClient) factsLike(ctx context.Context, repo, text string, limit int) (memory.Graph, error) {
	var res struct {
		memory.Graph
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/session-memory/recall",
		map[string]any{"query": clip(text, maxRecallQuery), "limit": limit}, &res)
	return res.Graph, err
}

// recallPrompt answers a UserPromptSubmit: facts that match the prompt and
// have not been shown in this chat yet.
func (o *recallOptions) recallPrompt(ctx context.Context, c apiClient, repo string, in hookInput, state string) (string, error) {
	g, err := c.factsLike(ctx, repo, in.Prompt, 10)
	if err != nil {
		return "", err
	}
	s := hookState{}
	if state != "" {
		s = loadState(state)
	}
	g = withoutFacts(g, s.Recalled)
	text := formatFactsOrdered(repo, g, promptRecallChars, "best match first")
	if text == "" {
		return "", nil
	}
	if state != "" {
		s.Recalled = trimTail(append(s.Recalled, factIDs(g)...), maxRecalled)
		saveState(state, s)
	}
	return text, nil
}

// recallCompacted answers the SessionStart that follows a compaction: what
// matches the work in progress first, then the newest facts, every fact once.
// The context was just emptied, so nothing counts as shown already.
func (o *recallOptions) recallCompacted(ctx context.Context, c apiClient, repo string, in hookInput, state string) (string, error) {
	newest, err := c.newestFacts(ctx, repo, o.limit)
	if err != nil {
		return "", err
	}
	var best memory.Graph
	if q := lastUserPrompt(in.TranscriptPath); q != "" && memory.RecallGate(q) == "" {
		// A failed lookup leaves the newest facts, which is still a recall.
		best, _ = c.factsLike(ctx, repo, q, 10) //nolint:errcheck // degrade to the newest facts
	}
	g := mergeFacts(best, newest)
	if state != "" {
		s := loadState(state)
		s.Recalled = trimTail(factIDs(g), maxRecalled)
		saveState(state, s)
	}
	// The matching facts come first, so the cap cuts the older ones.
	return formatFactsOrdered(repo, g, compactRecallChars, "best match first, then most recent"), nil
}

// lastUserPrompt is the latest thing the person asked that fits a query:
// the transcript's last user turn, skipping long ones (a compaction summary
// is a user turn too).
func lastUserPrompt(transcript string) string {
	if transcript == "" {
		return ""
	}
	f, err := os.Open(transcript) //nolint:gosec // the path Claude Code gave the hook
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // read-only
	turns, err := agentcfg.ReadTranscript(f)
	if err != nil {
		return ""
	}
	for _, t := range slices.Backward(turns) {
		if t.Role == "user" && strings.TrimSpace(t.Text) != "" && len(t.Text) <= 2000 {
			return clip(strings.TrimSpace(t.Text), maxRecallQuery)
		}
	}
	return ""
}

// mergeFacts is first's facts then rest's, each fact once, with the entities
// they name.
func mergeFacts(first, rest memory.Graph) memory.Graph {
	out := memory.Graph{Notice: cmpNotice(first, rest)}
	seen := map[string]bool{}
	for _, g := range []memory.Graph{first, rest} {
		out.Entities = append(out.Entities, g.Entities...)
		for _, f := range g.Facts {
			if !seen[f.ID] {
				seen[f.ID] = true
				out.Facts = append(out.Facts, f)
			}
		}
	}
	return out
}

func cmpNotice(gs ...memory.Graph) string {
	for _, g := range gs {
		if g.Notice != "" {
			return g.Notice
		}
	}
	return ""
}

// withoutFacts drops the facts whose IDs are in shown.
func withoutFacts(g memory.Graph, shown []string) memory.Graph {
	if len(shown) == 0 {
		return g
	}
	keep := g.Facts[:0:0]
	for _, f := range g.Facts {
		if !slices.Contains(shown, f.ID) {
			keep = append(keep, f)
		}
	}
	g.Facts = keep
	return g
}

func factIDs(g memory.Graph) []string {
	ids := make([]string, 0, len(g.Facts))
	for _, f := range g.Facts {
		ids = append(ids, f.ID)
	}
	return ids
}

func trimTail(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
