package pgstore

import (
	"context"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

var _ accounts.StoreSizer = (*Store)(nil)

// The statements are constants; the table list travels as $1.
const (
	sizeOfDatabase = `SELECT pg_database_size(current_database()), (SELECT count(*) FROM events), (SELECT count(*) FROM audit_segments)`
	sizeOfTables   = `SELECT c.relname, c.reltuples::bigint, pg_total_relation_size(c.oid)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND c.relname = ANY($1::text[])
ORDER BY c.relname`
)

// StoreSizes implements accounts.StoreSizer.
func (s *Store) StoreSizes(ctx context.Context) ([]telemetry.StoreSize, error) {
	var dbBytes, events, segments int64
	if err := s.db.QueryRow(ctx, sizeOfDatabase).Scan(&dbBytes, &events, &segments); err != nil {
		return nil, fmt.Errorf("database size: %w", err)
	}
	out := []telemetry.StoreSize{{System: telemetry.DBSystemPostgres, Kind: accounts.SizeKindDatabase, Rows: -1, Bytes: dbBytes}}
	rows, err := s.db.Query(ctx, sizeOfTables, accounts.SizedTables)
	if err != nil {
		return nil, fmt.Errorf("table sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x telemetry.StoreSize
		if err := rows.Scan(&x.Kind, &x.Rows, &x.Bytes); err != nil {
			return nil, fmt.Errorf("table sizes: %w", err)
		}
		x.System = telemetry.DBSystemPostgres
		switch x.Kind {
		case "events":
			x.Rows = events
		case "audit_segments":
			x.Rows = segments
		}
		// A table never analysed has an estimate of -1: not reported.
		x.Rows = max(x.Rows, -1)
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("table sizes: %w", err)
	}
	return out, nil
}
