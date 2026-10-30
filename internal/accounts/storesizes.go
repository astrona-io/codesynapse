package accounts

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// StoreSizer is an accounts store that reports its database size, the
// largest of a fixed list of tables and the audit log's hot rows and sealed
// segments (docs/plans/PLATFORM_STATS_PLAN.md, phase 6). The in-memory store has
// no database and does not implement it.
type StoreSizer interface {
	StoreSizes(ctx context.Context) ([]telemetry.StoreSize, error)
}

// SizeKindDatabase is the PostgreSQL kind of the whole database.
const SizeKindDatabase = "database"

// SizedTables are the tables whose size is sampled. The list is fixed: it
// is the gauge's kind allowlist and travels to PostgreSQL as one parameter.
// Rows are the planner's estimate, except events (the audit log's hot rows)
// and audit_segments (sealed files), which are counted.
var SizedTables = []string{
	"events", "audit_segments", "jobs", "sessions", "execution_sessions", "api_keys", "users", "orgs", "memberships",
	"repositories", "relation_tuples", "access_policy_outbox", "platform_stats_daily", "gateway_prompt_log",
	"ingest_runs", "image_scans", "login_attempts",
}

func init() {
	telemetry.AllowStoreKinds(telemetry.DBSystemPostgres, append([]string{SizeKindDatabase}, SizedTables...)...)
}
