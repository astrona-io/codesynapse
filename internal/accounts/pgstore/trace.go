package pgstore

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

// tracer times every statement for the metrics. The operation label is the
// statement's first keyword, never its text or arguments.
type tracer struct{}

type traceKey struct{}

type traceStart struct {
	at time.Time
	op string
}

func (tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceStart{at: time.Now(), op: operation(data.SQL)})
}

func (tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	st, ok := ctx.Value(traceKey{}).(traceStart)
	if !ok {
		return
	}
	tenant := telemetry.Tenant(ctx)
	telemetry.DBDuration.Observe(time.Since(st.at).Seconds(), tenant, "postgres", st.op)
	if data.Err != nil {
		telemetry.DBErrors.Inc(tenant, "postgres", st.op)
	}
}

func operation(sql string) string {
	f := strings.Fields(sql)
	if len(f) == 0 {
		return "other"
	}
	switch op := strings.ToLower(f[0]); op {
	case "select", "insert", "update", "delete", "with", "create", "alter", "begin", "commit", "rollback":
		return op
	}
	return "other"
}
