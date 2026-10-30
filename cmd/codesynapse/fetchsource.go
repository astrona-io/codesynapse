package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/fetchsource"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// fetch_source (internal/fetchsource) is answered here, on the developer's
// machine: the server never fetches a URL an agent names, so the tool reaches
// nothing the developer's own browser could not. Pages are cached in the
// user cache folder. The call is reported like the git tools, without the
// URL, its host or the checkout's name, plus how the fetch went and the
// page's sizes (sourceUsage) for the "External sources" savings.
//
// With a question and a pruned page still over fetchsource.ExtractThreshold
// characters, the page and the question go to the server's local-only
// source-extract task (tier 2) for the parts that answer it; any failure
// returns the pruned page with a note. CODESYNAPSE_SOURCE_EXTRACT=false
// keeps every page on this machine.

// capNote bounds a note made of server text.
func capNote(s string) string {
	if utf8.RuneCountInString(s) <= 300 {
		return s
	}
	return string([]rune(s)[:300]) + " …"
}

// sourceExtractTimeout bounds tier 2: a local model on a long page.
const sourceExtractTimeout = 2 * time.Minute

// fetchSourceTimeout bounds one answer: the fetch's own timeout, tier 2,
// the report and the cache.
const fetchSourceTimeout = fetchsource.Timeout + sourceExtractTimeout + 15*time.Second

// answerFetchSource answers a tools/call for fetch_source and reports
// whether it did.
func (b *bridge) answerFetchSource(ctx context.Context, msg []byte, m rpcMessage) bool {
	var c toolCall
	if json.Unmarshal(msg, &c) != nil || c.Params.Name != fetchsource.Tool || len(m.ID) == 0 || !b.mayAnswer(c.Params.Name) {
		return false
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, fetchSourceTimeout)
	defer cancel()

	result, src, failed := b.fetchSource(ctx, msg, c)
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	if err != nil {
		out = errorResponse(m.ID, err)
		failed = true
		src.Result = telemetry.SourceError
	}
	if !b.reportLocalSource(ctx, c.Params.Name, time.Since(start), len(out)/4, failed, &src) {
		return false
	}
	b.write(out)
	return true
}

func (b *bridge) fetchSource(ctx context.Context, msg []byte, c toolCall) (map[string]any, sourceUsage, bool) {
	ans, err := b.sourceFetcher().Run(ctx, c.Params.Arguments)
	src := sourceUsage{Result: fetchsource.Result(ans, err)}
	if err != nil {
		res, failed := localFail(err)
		return res, src, failed
	}
	var in fetchsource.In
	_ = json.Unmarshal(c.Params.Arguments, &in) //nolint:errcheck // Run decoded the same arguments strictly
	b.extractSource(ctx, msg, in.Question, &ans)
	raw, err := json.Marshal(ans)
	if err != nil {
		src.Result = telemetry.SourceError
		res, failed := localFail(err)
		return res, src, failed
	}
	// Clamped to what the server accepts: an out-of-range figure answers
	// 400, which the bridge takes for an older server and stops measuring.
	src.RawTokens = min(max(ans.RawTokens, 0), maxSourceTokens)
	src.CleanedTokens = min(max(ans.CleanedTokens, 0), maxSourceTokens)
	src.ExtractedTokens = min(max(ans.TextTokens, 0), maxSourceTokens)
	src.ReduceUs = min(max(ans.Reduce.Microseconds(), 0), maxSourceUs)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(raw)}},
		"structuredContent": json.RawMessage(raw),
		"_meta":             map[string]any{"codesynapse/source": "local-fetch"},
	}, src, false
}

// maxSourceTokens and maxSourceUs are the server's limits for one report
// (services/api/internal/api/usage_local.go).
const (
	maxSourceTokens = 1_000_000
	maxSourceUs     = 60 * 1_000_000
)

// sourceUsage is how one fetch_source call went, for the usage report: the
// result (hit, revalidated, miss, refused, error) and the page's estimated
// tokens as served, cleaned and returned, and the time cleaning took. Never
// the URL, its host, the question or the text.
type sourceUsage struct {
	Result          string `json:"result"`
	RawTokens       int    `json:"raw_tokens"`
	CleanedTokens   int    `json:"cleaned_tokens"`
	ExtractedTokens int    `json:"extracted_tokens"`
	ReduceUs        int64  `json:"reduce_us"`
}

// sourceTTLs reads the per-host TTLs from the user's config folder. A file
// that cannot be read or is invalid is reported once and ignored, so the
// built-in documentation list applies; the hosts it names are not logged.
func (b *bridge) sourceTTLs() *fetchsource.TTLs {
	path, err := fetchsource.DefaultTTLFile()
	if err != nil {
		return nil
	}
	ttls, err := fetchsource.LoadTTLs(path)
	if err != nil {
		b.warn("fetch_source TTL file ignored; the built-in TTLs apply", "error", err)
		return nil
	}
	return ttls
}

// sourceFetcher is the bridge's fetcher, made on first use. Without a usable
// cache folder pages are still fetched, just not kept.
func (b *bridge) sourceFetcher() *fetchsource.Fetcher {
	b.fetchOnce.Do(func() {
		if b.fetcher != nil {
			return
		}
		var cache *fetchsource.Cache
		dir, err := fetchsource.DefaultDir()
		if err == nil {
			cache, err = fetchsource.OpenCache(dir, fetchsource.DefaultCacheBytes)
		}
		if err != nil {
			b.warn("fetch_source cache unavailable; pages are not kept", "error", err)
		}
		b.fetcher = fetchsource.New(cache).WithTTLs(b.sourceTTLs())
	})
	return b.fetcher
}

// extractSource replaces ans.Text with the local model's extract for
// question when the pruned page is still long. The pruned page stands
// whenever there is no question, the page is JSON or short, tier 2 is off,
// the checkout is not indexed, the server has no local model (409) or the
// extract is not shorter.
func (b *bridge) extractSource(ctx context.Context, msg []byte, question string, ans *fetchsource.Out) {
	question = strings.TrimSpace(question)
	if question == "" || ans.ContentType == "application/json" || utf8.RuneCountInString(ans.Text) < fetchsource.ExtractThreshold ||
		app.Getenv("CODESYNAPSE_SOURCE_EXTRACT", "true") == "false" {
		return
	}
	if len(ans.Text) > fetchsource.MaxExtractInput {
		ans.ExtractNote = fmt.Sprintf("not shortened by a local model: the pruned page is over %d bytes", fetchsource.MaxExtractInput)
		return
	}
	if b.checkoutRepo(ctx) == "" {
		ans.ExtractNote = "not shortened by a local model: this checkout is not indexed in CodeSynapse"
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sourceExtractTimeout)
	defer cancel()
	var ex fetchsource.Extract
	if err := b.postLocal(ctx, msg, "source-extract", fetchsource.ExtractRequest{Question: question, Text: ans.Text}, &ex); err != nil {
		first, _, _ := strings.Cut(err.Error(), "\n")
		ans.ExtractNote = capNote("not shortened by a local model: " + first)
		return
	}
	// The server checked and redacted the extract; it is checked again here
	// because the bridge hands it to the agent.
	text := strings.TrimSpace(redact.Free(ex.Text))
	switch {
	case !ex.Found || text == "":
		ans.ExtractNote = "the local model found no answer to the question in the page; the pruned page is returned"
		return
	case len(text) >= len(ans.Text) || utf8.RuneCountInString(text) > fetchsource.MaxExtract+2:
		return
	}
	ans.Text, ans.Extracted, ans.ExtractModel, ans.ExtractDropped = text, true, ex.Model, ex.Dropped
	ans.TextBytes, ans.TextTokens = len(text), telemetry.EstimateTokens(len(text))
	if len(ex.Problems) > 0 {
		ans.ExtractNote = capNote(strings.Join(ex.Problems, "; "))
	}
}
