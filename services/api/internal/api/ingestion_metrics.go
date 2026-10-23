package api

import (
	"context"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

func (s *Server) exportIngestMetrics(ctx context.Context) {
	if s.cfg.IngestMetrics == nil {
		return
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.cfg.Accounts.St.ExportIngestTotals(work, func(exportCtx context.Context, saved accounts.IngestMetricSnapshot) error {
			snapshot := telemetry.IngestSnapshot{StreamID: saved.StreamID, StartedAt: saved.StartedAt, SampleAt: saved.SampleAt, DroppedSeries: saved.DroppedSeries}
			for _, total := range saved.Totals {
				item := telemetry.IngestTotal{TenantID: total.TenantID, RepositoryID: total.RepositoryID, StartedAt: total.StartedAt, Scanned: total.Scanned, Written: total.Written, Skipped: map[string]int64{}}
				for reason, count := range total.Skipped {
					item.Skipped[string(reason)] = count
				}
				snapshot.Totals = append(snapshot.Totals, item)
			}
			return s.cfg.IngestMetrics.Export(exportCtx, snapshot)
		})
		cancel()
		if err != nil && ctx.Err() == nil && !errors.Is(err, accounts.ErrIngestExporterBusy) {
			s.logger.Warn("ingest metrics export deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
