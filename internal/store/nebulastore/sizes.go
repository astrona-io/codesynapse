package nebulastore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Store sizes (docs/PLATFORM_STATS_PLAN.md, phase 6). NebulaGraph counts
// vertices per tag and edges per type only with its STATS job, which reads
// every partition. It runs at most once a day (the API gates it) and never
// while a publication holds a replace lease.

// Every statement here is a constant: no value reaches the text, and the
// job ID is compared in Go rather than written into SHOW JOB.
const (
	leasesHeld  = "MATCH (v:`ingest_head`) WHERE v.`ingest_head`.`stage_expires` > $now RETURN count(v) AS n"
	submitStats = "SUBMIT JOB STATS"
	showJobs    = "SHOW JOBS"
	showStats   = "SHOW STATS"
)

// statsJobWait bounds how long one sample waits for the STATS job.
const statsJobWait = 10 * time.Minute

var statsJobPoll = 2 * time.Second

// sizeKinds are the tag and edge type names the store keeps: the only
// kinds SHOW STATS rows are reported under.
var sizeKinds = func() map[string]bool {
	out := map[string]bool{}
	for _, it := range schemaItems {
		out[it.name] = true
	}
	return out
}()

func init() {
	kinds := make([]string, 0, len(sizeKinds))
	for k := range sizeKinds {
		kinds = append(kinds, k)
	}
	telemetry.AllowStoreKinds(telemetry.DBSystemNebula, kinds...)
}

var _ store.Sizer = (*Store)(nil)

// StoreSizes implements store.Sizer: vertices per tag and edges per type
// from a fresh STATS job, then the Milvus collections when there are any.
func (s *Store) StoreSizes(ctx context.Context) ([]telemetry.StoreSize, error) {
	if err := s.leaseFree(ctx); err != nil {
		return nil, err
	}
	rows, err := s.write(ctx, submitStats, nil)
	if err != nil {
		return nil, fmt.Errorf("start the stats job: %w", err)
	}
	if len(rows) != 1 {
		return nil, errors.New("start the stats job: no job id")
	}
	id, ok := rows[0]["New Job Id"].(int64)
	if !ok {
		return nil, errors.New("start the stats job: no job id")
	}
	if err := s.waitJob(ctx, id); err != nil {
		return nil, err
	}
	rows, err = s.read(ctx, showStats, nil)
	if err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}
	out := statsSizes(rows)
	if sz, ok := s.vec.(store.Sizer); ok {
		v, err := sz.StoreSizes(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, v...)
	}
	return out, nil
}

// leaseFree answers store.ErrSizesBusy while any replace (or session) lease
// is unexpired.
func (s *Store) leaseFree(ctx context.Context) error {
	rows, err := s.read(ctx, leasesHeld, map[string]any{"now": time.Now().UnixMilli()})
	if err != nil {
		return fmt.Errorf("check replace leases: %w", err)
	}
	if len(rows) == 1 {
		if n, _ := rows[0]["n"].(int64); n > 0 {
			return store.ErrSizesBusy
		}
	}
	return nil
}

// waitJob polls the constant SHOW JOBS until job id finished.
func (s *Store) waitJob(ctx context.Context, id int64) error {
	ctx, cancel := context.WithTimeout(ctx, statsJobWait)
	defer cancel()
	t := time.NewTicker(statsJobPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the stats job: %w", ctx.Err())
		case <-t.C:
		}
		rows, err := s.read(ctx, showJobs, nil)
		if err != nil {
			return fmt.Errorf("wait for the stats job: %w", err)
		}
		switch jobStatus(rows, id) {
		case "FINISHED":
			return nil
		case "FAILED", "STOPPED":
			return errors.New("the stats job did not finish")
		}
	}
}

func jobStatus(rows []Row, id int64) string {
	for _, r := range rows {
		if n, _ := r["Job Id"].(int64); n == id {
			st, _ := r["Status"].(string)
			return strings.ToUpper(st)
		}
	}
	return ""
}

// statsSizes keeps the SHOW STATS rows of this store's own tags and edge
// types, in name order; the space totals and anything else are left out.
func statsSizes(rows []Row) []telemetry.StoreSize {
	var out []telemetry.StoreSize
	for _, r := range rows {
		typ, _ := r["Type"].(string)
		name, _ := r["Name"].(string)
		n, _ := r["Count"].(int64)
		if (typ != "Tag" && typ != "Edge") || !sizeKinds[name] || n < 0 {
			continue
		}
		out = append(out, telemetry.StoreSize{System: telemetry.DBSystemNebula, Kind: name, Rows: n, Bytes: -1})
	}
	slices.SortFunc(out, func(a, b telemetry.StoreSize) int { return strings.Compare(a.Kind, b.Kind) })
	return slices.CompactFunc(out, func(a, b telemetry.StoreSize) bool { return a.Kind == b.Kind })
}
