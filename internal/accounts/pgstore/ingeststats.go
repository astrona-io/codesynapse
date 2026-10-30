package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// ingestStatsSQL sums the ingest runs of [$1, $2) per organisation
// (accounts.IngestInWindow): a finished run by its finish, or by its start
// once its report expired, a running one when it started before $2. File
// counts come from the runs that still have a report. It reads numbers and
// statuses only, never a graph name, run ID or file.
const ingestStatsSQL = `WITH w AS (
  SELECT r.org_id, r.status, r.terminal, r.report_json
  FROM ingest_runs r
  WHERE r.started_at < $2 AND (NOT r.terminal OR (
    COALESCE((r.report_json->>'finished_at')::timestamptz, r.started_at) >= $1 AND
    COALESCE((r.report_json->>'finished_at')::timestamptz, r.started_at) < $2))
)
SELECT w.org_id, EXISTS (SELECT 1 FROM orgs o WHERE o.id = w.org_id),
  count(*) FILTER (WHERE w.terminal AND w.status = 'succeeded'),
  count(*) FILTER (WHERE w.terminal AND w.status = 'failed'),
  count(*) FILTER (WHERE w.terminal AND w.status = 'cancelled'),
  count(*) FILTER (WHERE NOT w.terminal),
  count(*) FILTER (WHERE w.terminal AND w.report_json IS NULL),
  count(*) FILTER (WHERE w.terminal AND w.report_json IS NOT NULL AND NOT COALESCE((w.report_json->>'discovery_complete')::boolean, false)),
  COALESCE(sum((w.report_json->>'total_files_scanned')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->>'files_indexed')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->>'files_skipped')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->>'files_failed')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->>'files_unprocessed')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->'skip_breakdown'->>'ignored_path')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->'skip_breakdown'->>'generated_code')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->'skip_breakdown'->>'binary')::bigint) FILTER (WHERE w.terminal), 0),
  COALESCE(sum((w.report_json->'skip_breakdown'->>'non_k8s_yaml')::bigint) FILTER (WHERE w.terminal), 0)
FROM w GROUP BY w.org_id ORDER BY w.org_id`

// IngestStats implements accounts.Store.
func (s *Store) IngestStats(ctx context.Context, from, to time.Time) ([]accounts.OrgIngestStats, error) {
	var out []accounts.OrgIngestStats
	read := func(tx pgx.Tx) error {
		var err error
		out, err = ingestStats(ctx, tx, from, to)
		return err
	}
	var err error
	if s.policy {
		err = pgx.BeginFunc(ctx, s.db, read)
	} else {
		err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, read)
	}
	return out, err
}

func ingestStats(ctx context.Context, tx pgx.Tx, from, to time.Time) ([]accounts.OrgIngestStats, error) {
	out := []accounts.OrgIngestStats{}
	err := scanRows(ctx, tx, ingestStatsSQL, []any{from, to}, func(r pgx.Rows) error {
		var o accounts.OrgIngestStats
		f := &o.IngestFigures
		if err := r.Scan(&o.OrgID, &o.OrgExists, &f.Succeeded, &f.Failed, &f.Cancelled, &f.Running, &f.WithoutReport, &f.IncompleteDiscovery,
			&f.Scanned, &f.Indexed, &f.Skipped, &f.FilesFailed, &f.Unprocessed,
			&f.Skips.IgnoredPath, &f.Skips.GeneratedCode, &f.Skips.Binary, &f.Skips.NonK8sYAML); err != nil {
			return err
		}
		out = append(out, o)
		return nil
	})
	return out, err
}
