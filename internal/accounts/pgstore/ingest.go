package pgstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/jackc/pgx/v5"
)

// BeginIngestRun reserves a run and metric admission under the policy lock.
func (s *Store) BeginIngestRun(ctx context.Context, run accounts.IngestRun) error {
	run, hash, err := accounts.PrepareIngestRun(ctx, run)
	if err != nil {
		return err
	}
	if run.Report.Status != ingestreport.Queued || len(run.Report.Files) != 0 || run.Report.GraphCommit != ingestreport.CommitNone {
		return accounts.ErrIngestConflict
	}
	return s.PolicyTransaction(ctx, run.OrgID, func(st accounts.Store) error {
		if err := accounts.CheckIngestAuthority(ctx, st, run); err != nil {
			return err
		}
		tx := st.(*Store)
		var oldHash string
		var terminal bool
		err := tx.db.QueryRow(ctx, `SELECT report_hash,terminal FROM ingest_runs WHERE org_id=$1 AND graph=$2 AND run_id=$3`, run.OrgID, run.Graph, run.Report.RunID).Scan(&oldHash, &terminal)
		if err == nil {
			if oldHash == hash && !terminal {
				return nil
			}
			return accounts.ErrIngestConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var total, tenant, repo, pending, pendingTenant, pendingRepo int
		err = tx.db.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE org_id=$1),count(*) FILTER (WHERE org_id=$1 AND graph=$2),count(*) FILTER (WHERE NOT terminal),count(*) FILTER (WHERE NOT terminal AND org_id=$1),count(*) FILTER (WHERE NOT terminal AND org_id=$1 AND graph=$2) FROM ingest_runs`, run.OrgID, run.Graph).Scan(&total, &tenant, &repo, &pending, &pendingTenant, &pendingRepo)
		if err != nil {
			return err
		}
		if total >= accounts.MaxIngestRuns || tenant >= accounts.MaxTenantIngestRuns || repo >= accounts.MaxRepositoryIngestRuns || pending >= accounts.MaxPendingIngestRuns || pendingTenant >= accounts.MaxTenantPendingIngestRuns || pendingRepo > 0 {
			return accounts.ErrIngestLimit
		}
		if err := tx.initIngestStream(ctx); err != nil {
			return err
		}
		var exists bool
		if err := tx.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ingest_repository_totals WHERE org_id=$1 AND graph=$2)`, run.OrgID, run.Graph).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			var admitted, tenantAdmitted int
			if err := tx.db.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE org_id=$1) FROM ingest_repository_totals WHERE admitted`, run.OrgID).Scan(&admitted, &tenantAdmitted); err != nil {
				return err
			}
			id, err := accounts.NewIngestMetricID()
			if err != nil {
				return err
			}
			covered := admitted < accounts.MaxIngestMetricRepositories && tenantAdmitted < accounts.MaxTenantIngestMetricRepositories
			if _, err := tx.db.Exec(ctx, `INSERT INTO ingest_repository_totals(org_id,graph,repository_id,admitted,started_at) VALUES($1,$2,$3,$4,$5)`, run.OrgID, run.Graph, id, covered, time.Now().UTC().Truncate(time.Millisecond)); err != nil {
				return err
			}
			if !covered {
				if _, err := tx.db.Exec(ctx, `UPDATE ingest_metric_stream SET dropped_series=dropped_series+6`); err != nil {
					return err
				}
			}
		}
		identity, err := json.Marshal(run.Identity)
		if err != nil {
			return err
		}
		report, err := json.Marshal(run.Report)
		if err != nil {
			return err
		}
		_, err = tx.db.Exec(ctx, `INSERT INTO ingest_runs(org_id,graph,run_id,identity_json,report_json,report_hash,status,graph_commit,started_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, run.OrgID, run.Graph, run.Report.RunID, identity, report, hash, run.Report.Status, run.Report.GraphCommit, run.Report.StartedAt)
		if err != nil {
			return err
		}
		_, err = st.AddEvent(ctx, accounts.Event{UserID: run.Identity.SubjectID, OrgID: run.OrgID, Via: "execution", Action: "ingest.started", Target: run.Report.RunID, Outcome: accounts.Success})
		return err
	})
}

// SaveIngestRun atomically commits the report, cumulative totals and audit event.
func (s *Store) SaveIngestRun(ctx context.Context, run accounts.IngestRun) error {
	run, hash, err := accounts.PrepareIngestRun(ctx, run)
	if err != nil {
		return err
	}
	return s.PolicyTransaction(ctx, run.OrgID, func(st accounts.Store) error {
		if !accounts.IngestTerminal(run.Report.Status) || run.Report.Status == ingestreport.Succeeded {
			if err := accounts.CheckIngestAuthority(ctx, st, run); err != nil {
				return err
			}
		}
		return st.(*Store).saveIngest(ctx, run, hash, "execution")
	})
}
func (s *Store) saveIngest(ctx context.Context, run accounts.IngestRun, hash, via string) error {
	var oldHash string
	var identity, report []byte
	err := s.db.QueryRow(ctx, `SELECT identity_json,report_json,report_hash FROM ingest_runs WHERE org_id=$1 AND graph=$2 AND run_id=$3`, run.OrgID, run.Graph, run.Report.RunID).Scan(&identity, &report, &oldHash)
	if err != nil {
		return mapErr(err, "ingestion run")
	}
	previous := accounts.IngestRun{OrgID: run.OrgID, Graph: run.Graph}
	if err := json.Unmarshal(identity, &previous.Identity); err != nil {
		return err
	}
	if previous.Identity != run.Identity {
		return accounts.ErrIngestConflict
	}
	if oldHash == hash {
		return nil
	}
	if len(report) == 0 {
		return accounts.ErrIngestReportExpired
	}
	if err := json.Unmarshal(report, &previous.Report); err != nil {
		return err
	}
	if err := accounts.CheckIngestTransition(previous, run); err != nil {
		return err
	}
	report, err = json.Marshal(run.Report)
	if err != nil {
		return err
	}
	terminal := accounts.IngestTerminal(run.Report.Status)
	if _, err := s.db.Exec(ctx, `UPDATE ingest_runs SET report_json=$4,report_hash=$5,status=$6,graph_commit=$7,terminal=$8 WHERE org_id=$1 AND graph=$2 AND run_id=$3`, run.OrgID, run.Graph, run.Report.RunID, report, hash, run.Report.Status, run.Report.GraphCommit, terminal); err != nil {
		return err
	}
	skips := run.Report.SkipBreakdown
	oldSkips := previous.Report.SkipBreakdown
	_, err = s.db.Exec(ctx, `UPDATE ingest_repository_totals SET scanned=scanned+$3,written=written+$4,ignored_path=ignored_path+$5,generated_code=generated_code+$6,binary_files=binary_files+$7,non_k8s_yaml=non_k8s_yaml+$8,latest_success=CASE WHEN $9 THEN $10 ELSE latest_success END WHERE org_id=$1 AND graph=$2`, run.OrgID, run.Graph, run.Report.TotalFilesScanned-previous.Report.TotalFilesScanned, run.Report.FilesWritten-previous.Report.FilesWritten, skips[ingestreport.IgnoredPath]-oldSkips[ingestreport.IgnoredPath], skips[ingestreport.GeneratedCode]-oldSkips[ingestreport.GeneratedCode], skips[ingestreport.Binary]-oldSkips[ingestreport.Binary], skips[ingestreport.NonKubernetesYAML]-oldSkips[ingestreport.NonKubernetesYAML], run.Report.Status == ingestreport.Succeeded, run.Report.RunID)
	if err != nil {
		return err
	}
	if !terminal {
		return nil
	}
	_, err = s.db.Exec(ctx, `UPDATE ingest_runs SET report_json=NULL WHERE org_id=$1 AND graph=$2 AND run_id IN (SELECT run_id FROM ingest_runs WHERE org_id=$1 AND graph=$2 AND terminal AND report_json IS NOT NULL ORDER BY started_at DESC,run_id DESC OFFSET $3) AND run_id<>(SELECT latest_success FROM ingest_repository_totals WHERE org_id=$1 AND graph=$2)`, run.OrgID, run.Graph, accounts.IngestHistory)
	if err != nil {
		return err
	}
	var linked bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE org_id=$1 AND graph=$2)`, run.OrgID, run.Graph).Scan(&linked); err != nil {
		return err
	}
	if !linked {
		if err := s.expireIngestReports(ctx, run.OrgID, run.Graph); err != nil {
			return err
		}
	}
	_, err = s.AddEvent(ctx, accounts.Event{UserID: run.Identity.SubjectID, OrgID: run.OrgID, Via: via, Action: "ingest.completed", Target: run.Report.RunID, Outcome: accounts.Success, Detail: string(run.Report.Status) + " " + string(run.Report.GraphCommit)})
	return err
}

// ReconcileIngestRun accounts only a stored checkpoint and verified receipt.
func (s *Store) ReconcileIngestRun(ctx context.Context, org, graph, id string, commit ingestreport.GraphCommit) error {
	return s.PolicyTransaction(ctx, org, func(st accounts.Store) error {
		run, err := st.GetIngestRun(ctx, org, graph, id)
		if err != nil {
			return err
		}
		if accounts.IngestTerminal(run.Report.Status) {
			if run.Report.GraphCommit == commit {
				return nil
			}
			return accounts.ErrIngestConflict
		}
		run, err = accounts.FinishIngestRecovery(run, commit)
		if err != nil {
			return err
		}
		b, err := json.Marshal(run.Report)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		return st.(*Store).saveIngest(ctx, run, hex.EncodeToString(sum[:]), "recovery")
	})
}

const ingestRunColumns = `r.org_id,r.graph,r.identity_json,r.report_json,t.repository_id,t.admitted,m.last_exported_at`
const ingestSummaryColumns = `r.org_id,r.graph,r.identity_json,r.report_json - 'files',t.repository_id,t.admitted,m.last_exported_at`
const ingestRunFrom = ` FROM ingest_runs r JOIN ingest_repository_totals t ON t.org_id=r.org_id AND t.graph=r.graph CROSS JOIN ingest_metric_stream m `

func scanIngestRun(row pgx.Row) (accounts.IngestRun, error) {
	var run accounts.IngestRun
	var identity, report []byte
	err := row.Scan(&run.OrgID, &run.Graph, &identity, &report, &run.RepositoryID, &run.MetricCoverage, &run.LastExportedAt)
	if err != nil {
		return run, mapErr(err, "ingestion run")
	}
	if len(report) == 0 {
		return run, accounts.ErrIngestReportExpired
	}
	if err := json.Unmarshal(identity, &run.Identity); err != nil {
		return run, err
	}
	if err := json.Unmarshal(report, &run.Report); err != nil {
		return run, err
	}
	run.LastExportedAt = run.LastExportedAt.UTC()
	return run, nil
}

// GetIngestRun scopes both report and retained tombstone to the tenant entry.
func (s *Store) GetIngestRun(ctx context.Context, org, graph, id string) (accounts.IngestRun, error) {
	return scanIngestRun(s.db.QueryRow(ctx, `SELECT `+ingestRunColumns+ingestRunFrom+`WHERE r.org_id=$1 AND r.graph=$2 AND r.run_id=$3`, org, graph, id))
}

// ListIngestRuns includes the separately pinned success within its bound.
func (s *Store) ListIngestRuns(ctx context.Context, org, graph string, limit int) ([]accounts.IngestRun, error) {
	rows, err := s.db.Query(ctx, `SELECT `+ingestSummaryColumns+ingestRunFrom+`WHERE r.org_id=$1 AND r.graph=$2 AND r.report_json IS NOT NULL ORDER BY r.started_at DESC,r.run_id DESC LIMIT $3`, org, graph, min(max(limit, 1), accounts.IngestHistory+1))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (accounts.IngestRun, error) { return scanIngestRun(row) })
}

// LatestSuccessfulIngestRun survives subsequent failed attempts and retention.
func (s *Store) LatestSuccessfulIngestRun(ctx context.Context, org, graph string) (accounts.IngestRun, error) {
	return scanIngestRun(s.db.QueryRow(ctx, `SELECT `+ingestSummaryColumns+ingestRunFrom+`WHERE r.org_id=$1 AND r.graph=$2 AND r.run_id=t.latest_success`, org, graph))
}

// PendingIngestRuns is bounded and reserved for internal reconciliation.
func (s *Store) PendingIngestRuns(ctx context.Context, graph string, limit int) ([]accounts.IngestRun, error) {
	rows, err := s.db.Query(ctx, `SELECT `+ingestRunColumns+ingestRunFrom+`WHERE NOT r.terminal AND ($1='' OR r.graph=$1) ORDER BY r.started_at,r.run_id LIMIT $2`, graph, min(max(limit, 1), accounts.MaxPendingIngestRuns))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (accounts.IngestRun, error) { return scanIngestRun(row) })
}
func (s *Store) initIngestStream(ctx context.Context) error {
	id, err := accounts.NewIngestMetricID()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO ingest_metric_stream(singleton,stream_id,started_at,sample_at,last_exported_at) VALUES(true,$1,$2,$3,$3) ON CONFLICT(singleton) DO NOTHING`, id, time.Now().UTC().Truncate(time.Millisecond), time.Time{})
	return err
}

// ExportIngestTotals holds a distinct session lock across IO. The sample time
// commits before sending, so a delayed former owner cannot publish a newer point.
func (s *Store) ExportIngestTotals(ctx context.Context, export func(context.Context, accounts.IngestMetricSnapshot) error) error {
	if export == nil || s.policy {
		return accounts.ErrInvalidInput
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(1668244583)`).Scan(&locked); err != nil {
		return err
	}
	if !locked {
		return accounts.ErrIngestExporterBusy
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(1668244583)`); err != nil {
			_ = conn.Conn().Close(unlockCtx)
		}
	}()
	var snapshot accounts.IngestMetricSnapshot
	err = s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		tx := st.(*Store)
		if err := tx.initIngestStream(ctx); err != nil {
			return err
		}
		err := tx.db.QueryRow(ctx, `UPDATE ingest_metric_stream SET sample_at=GREATEST($1,sample_at+interval '1 millisecond') RETURNING stream_id,started_at,sample_at,last_exported_at,dropped_series`, time.Now().UTC().Truncate(time.Millisecond)).Scan(&snapshot.StreamID, &snapshot.StartedAt, &snapshot.SampleAt, &snapshot.LastExportedAt, &snapshot.DroppedSeries)
		if err != nil {
			return err
		}
		snapshot.StartedAt = snapshot.StartedAt.UTC()
		snapshot.SampleAt = snapshot.SampleAt.UTC()
		snapshot.LastExportedAt = snapshot.LastExportedAt.UTC()
		rows, err := tx.db.Query(ctx, `SELECT org_id,repository_id,started_at,scanned,written,ignored_path,generated_code,binary_files,non_k8s_yaml FROM ingest_repository_totals WHERE admitted ORDER BY repository_id`)
		if err != nil {
			return err
		}
		snapshot.Totals, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (accounts.IngestMetricTotal, error) {
			var total accounts.IngestMetricTotal
			var ignored, generated, binary, nonK8s int64
			err := row.Scan(&total.TenantID, &total.RepositoryID, &total.StartedAt, &total.Scanned, &total.Written, &ignored, &generated, &binary, &nonK8s)
			total.StartedAt = total.StartedAt.UTC()
			total.Skipped = map[ingestreport.SkipReason]int64{ingestreport.IgnoredPath: ignored, ingestreport.GeneratedCode: generated, ingestreport.Binary: binary, ingestreport.NonKubernetesYAML: nonK8s}
			return total, err
		})
		return err
	})
	if err != nil {
		return err
	}
	if err := export(ctx, snapshot); err != nil {
		return err
	}
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		_, err := st.(*Store).db.Exec(ctx, `UPDATE ingest_metric_stream SET last_exported_at=GREATEST(last_exported_at,$1)`, snapshot.SampleAt)
		return err
	})
}

func (s *Store) expireIngestReports(ctx context.Context, org, graph string) error {
	if _, err := s.db.Exec(ctx, `UPDATE ingest_runs SET report_json=NULL WHERE org_id=$1 AND graph=$2 AND terminal`, org, graph); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE ingest_repository_totals SET latest_success='' WHERE org_id=$1 AND graph=$2`, org, graph)
	return err
}
