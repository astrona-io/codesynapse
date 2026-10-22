package memstore

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
)

type ingestRecord struct {
	run                accounts.IngestRun
	hash               string
	terminal, retained bool
}
type ingestEntry struct {
	total         accounts.IngestMetricTotal
	admitted      bool
	latestSuccess string
}

func ingestKey(org, graph, id string) string {
	b, _ := json.Marshal([]string{org, graph, id})
	return string(b)
}

// BeginIngestRun reserves an immutable run before reading repository contents.
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
		created, err := tx.beginIngest(run, hash)
		if err != nil || !created {
			return err
		}
		_, err = st.AddEvent(ctx, accounts.Event{UserID: run.Identity.SubjectID, OrgID: run.OrgID, Via: "execution", Action: "ingest.started", Target: run.Report.RunID, Outcome: accounts.Success})
		return err
	})
}
func (s *Store) beginIngest(run accounts.IngestRun, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ingestKey(run.OrgID, run.Graph, run.Report.RunID)
	if old, ok := s.ingestRuns[key]; ok {
		if old.hash == hash && !old.terminal {
			return false, nil
		}
		return false, accounts.ErrIngestConflict
	}
	retainedTenant, retainedRepo, pending, pendingTenant, pendingRepo := 0, 0, 0, 0, 0
	for _, old := range s.ingestRuns {
		if old.run.OrgID == run.OrgID {
			retainedTenant++
			if old.run.Graph == run.Graph {
				retainedRepo++
			}
		}
		if !old.terminal {
			pending++
			if old.run.OrgID == run.OrgID {
				pendingTenant++
				if old.run.Graph == run.Graph {
					pendingRepo++
				}
			}
		}
	}
	if len(s.ingestRuns) >= accounts.MaxIngestRuns || retainedTenant >= accounts.MaxTenantIngestRuns || retainedRepo >= accounts.MaxRepositoryIngestRuns || pending >= accounts.MaxPendingIngestRuns || pendingTenant >= accounts.MaxTenantPendingIngestRuns || pendingRepo > 0 {
		return false, accounts.ErrIngestLimit
	}
	entryKey := ingestKey(run.OrgID, run.Graph, "")
	entry, ok := s.ingestEntries[entryKey]
	if !ok {
		id, err := accounts.NewIngestMetricID()
		if err != nil {
			return false, err
		}
		admitted, tenantAdmitted := 0, 0
		for _, e := range s.ingestEntries {
			if e.admitted {
				admitted++
				if e.total.TenantID == run.OrgID {
					tenantAdmitted++
				}
			}
		}
		entry = ingestEntry{total: accounts.IngestMetricTotal{TenantID: run.OrgID, RepositoryID: id, StartedAt: time.Now().UTC().Truncate(time.Millisecond), Skipped: map[ingestreport.SkipReason]int64{}}, admitted: admitted < accounts.MaxIngestMetricRepositories && tenantAdmitted < accounts.MaxTenantIngestMetricRepositories}
		if !entry.admitted {
			s.ingestMetrics.DroppedSeries += 6
		}
		s.ingestEntries[entryKey] = entry
	}
	if s.ingestMetrics.StreamID == "" {
		id, err := accounts.NewIngestMetricID()
		if err != nil {
			return false, err
		}
		s.ingestMetrics.StreamID = id
		s.ingestMetrics.StartedAt = time.Now().UTC().Truncate(time.Millisecond)
	}
	run.RepositoryID, run.MetricCoverage = entry.total.RepositoryID, entry.admitted
	s.ingestRuns[key] = ingestRecord{run: run, hash: hash, retained: true}
	return true, nil
}

// SaveIngestRun persists a checkpoint or accounts a final outcome once.
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
		changed, err := st.(*Store).saveIngest(run, hash)
		if err != nil {
			return err
		}
		if changed && accounts.IngestTerminal(run.Report.Status) {
			_, err = st.AddEvent(ctx, accounts.Event{UserID: run.Identity.SubjectID, OrgID: run.OrgID, Via: "execution", Action: "ingest.completed", Target: run.Report.RunID, Outcome: accounts.Success, Detail: string(run.Report.Status) + " " + string(run.Report.GraphCommit)})
		}
		return err
	})
}
func (s *Store) saveIngest(run accounts.IngestRun, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ingestKey(run.OrgID, run.Graph, run.Report.RunID)
	old, ok := s.ingestRuns[key]
	if !ok {
		return false, accounts.ErrNotFound
	}
	if old.run.Identity != run.Identity {
		return false, accounts.ErrIngestConflict
	}
	if old.hash == hash {
		return false, nil
	}
	if !old.retained {
		return false, accounts.ErrIngestReportExpired
	}
	if err := accounts.CheckIngestTransition(old.run, run); err != nil {
		return false, err
	}
	entryKey := ingestKey(run.OrgID, run.Graph, "")
	entry := s.ingestEntries[entryKey]
	run.RepositoryID, run.MetricCoverage = entry.total.RepositoryID, entry.admitted
	terminal := accounts.IngestTerminal(run.Report.Status)
	entry.total.Skipped = maps.Clone(entry.total.Skipped)
	entry.total.Scanned += int64(run.Report.TotalFilesScanned - old.run.Report.TotalFilesScanned)
	entry.total.Written += int64(run.Report.FilesWritten - old.run.Report.FilesWritten)
	for reason, n := range run.Report.SkipBreakdown {
		entry.total.Skipped[reason] += int64(n - old.run.Report.SkipBreakdown[reason])
	}
	if run.Report.Status == ingestreport.Succeeded {
		entry.latestSuccess = run.Report.RunID
	}
	s.ingestEntries[entryKey] = entry
	s.ingestRuns[key] = ingestRecord{run: run, hash: hash, terminal: terminal, retained: true}
	if terminal {
		s.pruneIngest(run.OrgID, run.Graph, entry.latestSuccess)
		if _, exists := s.repos[run.OrgID+"|"+run.Graph]; !exists {
			s.expireIngestReports(run.OrgID, run.Graph)
		}
	}
	return true, nil
}
func (s *Store) pruneIngest(org, graph, pin string) {
	keys := []string{}
	for key, r := range s.ingestRuns {
		if r.retained && r.terminal && r.run.OrgID == org && r.run.Graph == graph {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(s.ingestRuns[b].run.Report.StartedAt.Compare(s.ingestRuns[a].run.Report.StartedAt), cmp.Compare(s.ingestRuns[b].run.Report.RunID, s.ingestRuns[a].run.Report.RunID))
	})
	for _, key := range keys[min(len(keys), accounts.IngestHistory):] {
		r := s.ingestRuns[key]
		if r.run.Report.RunID == pin {
			continue
		}
		r.run.Report.Files = nil
		r.run.Report.SkipBreakdown = nil
		r.retained = false
		s.ingestRuns[key] = r
	}
}

// ReconcileIngestRun records a verified receipt without granting content access.
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
		changed, err := st.(*Store).saveIngest(run, hex.EncodeToString(sum[:]))
		if err != nil {
			return err
		}
		if changed {
			_, err = st.AddEvent(ctx, accounts.Event{UserID: run.Identity.SubjectID, OrgID: org, Via: "recovery", Action: "ingest.completed", Target: id, Outcome: accounts.Success, Detail: string(run.Report.Status) + " " + string(commit)})
		}
		return err
	})
}

// GetIngestRun never returns another tenant's run or a pruned report.
func (s *Store) GetIngestRun(_ context.Context, org, graph, id string) (accounts.IngestRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.ingestRuns[ingestKey(org, graph, id)]
	if !ok {
		return accounts.IngestRun{}, accounts.ErrNotFound
	}
	if !r.retained {
		return accounts.IngestRun{}, accounts.ErrIngestReportExpired
	}
	return s.ingestCopy(r.run), nil
}
func (s *Store) ingestCopy(run accounts.IngestRun) accounts.IngestRun {
	run.Report = run.Report.Clone()
	run.LastExportedAt = s.ingestMetrics.LastExportedAt
	return run
}

// ListIngestRuns returns retained attempts newest first.
func (s *Store) ListIngestRuns(_ context.Context, org, graph string, limit int) ([]accounts.IngestRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.IngestRun{}
	for _, r := range s.ingestRuns {
		if r.retained && r.run.OrgID == org && r.run.Graph == graph {
			run := r.run
			run.Report.Files = nil
			out = append(out, s.ingestCopy(run))
		}
	}
	slices.SortFunc(out, func(a, b accounts.IngestRun) int {
		return cmp.Or(b.Report.StartedAt.Compare(a.Report.StartedAt), cmp.Compare(b.Report.RunID, a.Report.RunID))
	})
	return out[:min(len(out), min(max(limit, 1), accounts.IngestHistory+1))], nil
}

// LatestSuccessfulIngestRun retains the success independently of failed attempts.
func (s *Store) LatestSuccessfulIngestRun(_ context.Context, org, graph string) (accounts.IngestRun, error) {
	s.mu.RLock()
	entry := s.ingestEntries[ingestKey(org, graph, "")]
	defer s.mu.RUnlock()
	if entry.latestSuccess == "" {
		return accounts.IngestRun{}, accounts.ErrNotFound
	}
	record, ok := s.ingestRuns[ingestKey(org, graph, entry.latestSuccess)]
	if !ok || !record.retained {
		return accounts.IngestRun{}, accounts.ErrNotFound
	}
	run := record.run
	run.Report.Files = nil
	return s.ingestCopy(run), nil
}

// PendingIngestRuns is an internal, bounded reconciliation queue.
func (s *Store) PendingIngestRuns(_ context.Context, graph string, limit int) ([]accounts.IngestRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.IngestRun{}
	for _, r := range s.ingestRuns {
		if !r.terminal && r.retained && (graph == "" || r.run.Graph == graph) {
			out = append(out, s.ingestCopy(r.run))
		}
	}
	slices.SortFunc(out, func(a, b accounts.IngestRun) int {
		return cmp.Or(a.Report.StartedAt.Compare(b.Report.StartedAt), cmp.Compare(a.Report.RunID, b.Report.RunID))
	})
	return out[:min(len(out), min(max(limit, 1), accounts.MaxPendingIngestRuns))], nil
}

// ExportIngestTotals permits one sender and persists its sample time before IO.
func (s *Store) ExportIngestTotals(ctx context.Context, export func(context.Context, accounts.IngestMetricSnapshot) error) error {
	if export == nil || s.policy {
		return accounts.ErrInvalidInput
	}
	if !s.ingestExportMu.TryLock() {
		return accounts.ErrIngestExporterBusy
	}
	defer s.ingestExportMu.Unlock()
	var snapshot accounts.IngestMetricSnapshot
	err := s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		tx := st.(*Store)
		tx.mu.Lock()
		defer tx.mu.Unlock()
		if tx.ingestMetrics.StreamID == "" {
			id, err := accounts.NewIngestMetricID()
			if err != nil {
				return err
			}
			tx.ingestMetrics.StreamID = id
			tx.ingestMetrics.StartedAt = time.Now().UTC().Truncate(time.Millisecond)
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		if !now.After(tx.ingestMetrics.SampleAt) {
			now = tx.ingestMetrics.SampleAt.Add(time.Millisecond)
		}
		tx.ingestMetrics.SampleAt = now
		snapshot = tx.ingestMetrics
		snapshot.Totals = []accounts.IngestMetricTotal{}
		for _, entry := range tx.ingestEntries {
			if entry.admitted {
				total := entry.total
				total.Skipped = maps.Clone(total.Skipped)
				snapshot.Totals = append(snapshot.Totals, total)
			}
		}
		slices.SortFunc(snapshot.Totals, func(a, b accounts.IngestMetricTotal) int { return cmp.Compare(a.RepositoryID, b.RepositoryID) })
		return nil
	})
	if err != nil {
		return err
	}
	if err := export(ctx, snapshot); err != nil {
		return err
	}
	return s.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		tx := st.(*Store)
		tx.mu.Lock()
		defer tx.mu.Unlock()
		tx.ingestMetrics.LastExportedAt = snapshot.SampleAt
		return nil
	})
}

// expireIngestReports retains pending recovery checkpoints and replay identity.
// The caller holds the store mutex.
func (s *Store) expireIngestReports(org, graph string) {
	for key, r := range s.ingestRuns {
		if r.terminal && r.run.OrgID == org && r.run.Graph == graph {
			r.retained = false
			r.run.Report.Files = nil
			r.run.Report.SkipBreakdown = nil
			s.ingestRuns[key] = r
		}
	}
	key := ingestKey(org, graph, "")
	entry, ok := s.ingestEntries[key]
	if ok {
		entry.latestSuccess = ""
		s.ingestEntries[key] = entry
	}
}
