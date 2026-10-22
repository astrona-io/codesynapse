package pgstore

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestTraceRetainsTrustedAttributionAndCountsTimeout(t *testing.T) {
	const tenant = "cccccccccccccccccccccccc"
	telemetry.AllowTools("pg_trace_test")
	telemetry.AllowModels("configured", "configured-model")
	ctx := telemetry.WithModel(telemetry.WithTool(telemetry.WithTenant(t.Context(), tenant), "pg_trace_test"), "configured", "configured-model")
	tr := tracer{}
	ctx = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT secret_input_from_client"})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: context.DeadlineExceeded})
	var text strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&text))
	labels := `{tenant_id="` + tenant + `",db_system="postgres",operation="select",mcp_tool="pg_trace_test",llm_provider="configured",llm_model="configured-model"}`
	assert.Contains(t, text.String(), "db_query_errors_total"+labels+" 1")
	assert.Contains(t, text.String(), "db_query_timeouts_total"+labels+" 1")
	assert.Contains(t, text.String(), "db_query_duration_seconds_count"+labels+" 1")
	assert.False(t, strings.Contains(text.String(), "secret_input_from_client"))
}
