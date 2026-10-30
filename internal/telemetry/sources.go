package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Results of codesynapse_source_fetches_total: a page fetch_source returned
// from its cache (hit), after a 304 (revalidated) or from a new download
// (miss), a URL or address it would not fetch (refused), and a page it could
// not get (error).
const (
	SourceHit         = "hit"
	SourceRevalidated = "revalidated"
	SourceMiss        = "miss"
	SourceRefused     = "refused"
	SourceError       = "error"
)

// SourceResults is the fixed set of results.
var SourceResults = []string{SourceHit, SourceRevalidated, SourceMiss, SourceRefused, SourceError}

// Stages of codesynapse_source_tokens_total: the page as served (raw), its
// cleaned text (what the cache keeps) and the text returned to the agent
// (extracted: the sections that answer the question today, a local model's
// extract once tier 2 exists).
const (
	StageRaw       = "raw"
	StageCleaned   = "cleaned"
	StageExtracted = "extracted"
)

// Tiers of codesynapse_source_reduce_seconds.
const (
	TierCPU        = "cpu"
	TierLocalModel = "local_model"
)

// SourceFetch is one fetch_source answer as the bridge reported it: counts
// and times, never the URL, its host or the question. Reduce is tier 1
// (cleaning and pruning, cpu); Extract is tier 2 (the local model's
// source-extract call, local_model), zero when the model was not asked.
type SourceFetch struct {
	Result                                    string
	RawTokens, CleanedTokens, ExtractedTokens int
	Reduce, Extract                           time.Duration
}

// RecordSourceFetch counts one fetch_source answer under tool and adds the
// result, tier and sizes to the request's span. An unknown result is not
// recorded; tokens and the reduce times count only for a page returned. The
// span's tier is the last one that ran.
func RecordSourceFetch(ctx context.Context, tool string, f SourceFetch) {
	switch f.Result {
	case SourceHit, SourceRevalidated, SourceMiss, SourceRefused, SourceError:
	default:
		return
	}
	ctx = WithTool(ctx, tool)
	SourceFetches.IncContext(ctx, f.Result)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("source.result", f.Result))
	if f.Result == SourceRefused || f.Result == SourceError {
		return
	}
	for _, s := range []struct {
		stage  string
		tokens int
	}{{StageRaw, f.RawTokens}, {StageCleaned, f.CleanedTokens}, {StageExtracted, f.ExtractedTokens}} {
		if s.tokens > 0 {
			SourceTokens.AddContext(ctx, float64(s.tokens), s.stage)
		}
	}
	SourceReduce.ObserveContext(ctx, f.Reduce.Seconds(), TierCPU)
	tier := TierCPU
	if f.Extract > 0 {
		SourceReduce.ObserveContext(ctx, f.Extract.Seconds(), TierLocalModel)
		tier = TierLocalModel
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("source.tier", tier),
		attribute.Int("source.raw_tokens", f.RawTokens), attribute.Int("source.cleaned_tokens", f.CleanedTokens),
		attribute.Int("source.extracted_tokens", f.ExtractedTokens))
}
