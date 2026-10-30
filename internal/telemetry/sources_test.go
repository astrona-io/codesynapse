package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestSourceFetchesStayInTheirSets(t *testing.T) {
	AllowTools("fetch_source")
	ctx := WithTenant(context.Background(), "eeeeeeeeeeeeeeeeeeeeeeee")
	RecordSourceFetch(ctx, "fetch_source", SourceFetch{Result: SourceMiss, RawTokens: 4000, CleanedTokens: 900, ExtractedTokens: 300, Reduce: 3 * time.Millisecond})
	RecordSourceFetch(ctx, "fetch_source", SourceFetch{Result: SourceHit, RawTokens: 4000, CleanedTokens: 900, ExtractedTokens: 900, Tier: "gpu-cluster"})
	RecordSourceFetch(ctx, "fetch_source", SourceFetch{Result: SourceRefused, RawTokens: 77777})
	RecordSourceFetch(ctx, "fetch_source", SourceFetch{Result: "https://example.com/docs"})

	out := text(t, Default)
	const id = `tenant_id="eeeeeeeeeeeeeeeeeeeeeeee"`
	assert.Contains(t, out, `codesynapse_source_fetches_total{result="miss",`+id+`,mcp_tool="fetch_source"`)
	assert.Contains(t, out, `codesynapse_source_fetches_total{result="hit",`+id)
	assert.Contains(t, out, `codesynapse_source_fetches_total{result="refused",`+id)
	assert.Contains(t, out, `codesynapse_source_tokens_total{stage="raw",`+id)
	assert.Contains(t, out, `codesynapse_source_tokens_total{stage="extracted",`+id)
	assert.Contains(t, out, `codesynapse_source_reduce_seconds_count{tier="cpu",`+id)
	assert.False(t, strings.Contains(out, "example.com"), "results come from a fixed set")
	assert.False(t, strings.Contains(out, "gpu-cluster"), "tiers come from a fixed set")
	assert.False(t, strings.Contains(out, "77777"), "a refused fetch counts no tokens")
}
