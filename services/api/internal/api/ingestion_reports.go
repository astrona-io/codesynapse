package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// The shared graph contains only an opaque commit receipt. Counts and execution
// provenance stay in the tenant's accounts record.
func ingestReceipt(org, graph, run string) string {
	b, _ := json.Marshal([]string{org, graph, run})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func attachIngestReceipt(g *model.Graph, org, graph, run string) error {
	n, ok := g.Node(model.NodeID(graph, model.KindRepository, graph))
	if !ok {
		return errors.New("ingest graph has no repository")
	}
	meta := map[string]json.RawMessage{}
	if n.Meta != "" {
		if err := json.Unmarshal([]byte(n.Meta), &meta); err != nil {
			return err
		}
	}
	if meta == nil {
		meta = make(map[string]json.RawMessage)
	}
	value, _ := json.Marshal(ingestReceipt(org, graph, run))
	meta["ingest_receipt"] = value
	data, err := json.Marshal(meta)
	if err == nil {
		n.Meta = string(data)
	}
	return err
}

func (s *Server) receiptMatches(ctx context.Context, run accounts.IngestRun) (bool, error) {
	n, err := s.st.Node(ctx, model.NodeID(run.Graph, model.KindRepository, run.Graph))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var meta struct {
		Receipt string `json:"ingest_receipt"`
	}
	if n.Meta != "" {
		if err := json.Unmarshal([]byte(n.Meta), &meta); err != nil {
			return false, err
		}
	}
	return meta.Receipt == ingestReceipt(run.OrgID, run.Graph, run.Report.RunID), nil
}

func (s *Server) saveIngestReport(ctx context.Context, job Job, report ingestreport.Report) error {
	if s.cfg.Accounts == nil {
		return nil
	}
	run := accounts.IngestRun{OrgID: job.OrgID, Graph: job.Repo, Identity: job.Identity, Report: report}
	service := s.accountsFor(ctx)
	if report.Status == ingestreport.Queued {
		return service.St.BeginIngestRun(ctx, run)
	}
	if report.GraphCommit == ingestreport.CommitUnknown {
		identity, err := execution.Require(ctx)
		if err != nil {
			return err
		}
		if identity != job.Identity {
			return execution.ErrConflict
		}
		// This callback runs after the writer ended. A failed read keeps the
		// prepared ledger pending so a later reconciliation can still confirm it.
		return service.St.PolicyTransaction(ctx, job.OrgID, func(st accounts.Store) error {
			pending, err := st.GetIngestRun(ctx, job.OrgID, job.Repo, job.ID)
			if err != nil {
				return err
			}
			if pending.Identity != job.Identity {
				return execution.ErrConflict
			}
			if pending.Report.Status == ingestreport.Succeeded {
				return nil
			}
			matched, err := s.receiptMatches(ctx, pending)
			if err != nil {
				return err
			}
			commit := ingestreport.CommitUnknown
			if matched {
				commit = ingestreport.CommitCommitted
			}
			return st.ReconcileIngestRun(ctx, job.OrgID, job.Repo, job.ID, commit)
		})
	}
	if err := service.St.SaveIngestRun(ctx, run); err != nil {
		return err
	}
	if report.FinishedAt != nil {
		completed := func() {
			s.logger.Info("ingest accounting completed", "job", job.ID, "repo", job.Repo,
				"scanned", report.TotalFilesScanned, "indexed", report.FilesIndexed,
				"written", report.FilesWritten, "reused", report.FilesReused,
				"skipped", report.FilesSkipped, "failed", report.FilesFailed,
				"unprocessed", report.FilesUnprocessed, "discovery_complete", report.DiscoveryComplete)
		}
		if tx, ok := ctx.Value(accountTransactionKey{}).(*accountTransaction); ok && tx.server == s {
			tx.afterCommit = append(tx.afterCommit, completed)
		} else {
			completed()
		}
	}
	return nil
}

// Reconciliation holds the same lock as graph publication. It never authorizes
// work or retries a write; it only accounts for an already committed receipt.
func (s *Server) reconcileIngestReports(ctx context.Context, graph string) error {
	return s.accountsFor(ctx).St.PolicyTransaction(ctx, "", func(st accounts.Store) error {
		runs, err := st.PendingIngestRuns(ctx, graph, 16)
		if err != nil {
			return err
		}
		for _, run := range runs {
			matched, err := s.receiptMatches(ctx, run)
			if err != nil {
				return err
			}
			if matched && run.Report.Status == ingestreport.Prepared {
				if err := st.ReconcileIngestRun(ctx, run.OrgID, run.Graph, run.Report.RunID, ingestreport.CommitCommitted); err != nil {
					return err
				}
			} else if time.Since(run.Report.StartedAt) > 2*time.Hour {
				// Jobs have a one-hour lifetime. An older unmatched run has no
				// active writer; its past commit cannot be proven.
				if err := st.ReconcileIngestRun(ctx, run.OrgID, run.Graph, run.Report.RunID, ingestreport.CommitUnknown); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Server) recoverIngestReports(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.reconcileIngestReports(work, "")
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("ingest accounting recovery deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) ingestionRoutes() {
	s.route("GET /api/v1/repos/{repo}/ingestion", repoRole(accounts.Viewer), s.handleIngestion)
	s.route("GET /api/v1/repos/{repo}/ingestion/{run}", repoRole(accounts.Viewer), s.handleIngestRun)
}

func ingestSummary(run accounts.IngestRun) accounts.IngestRun {
	run.Report.Files = nil
	return run
}

func (s *Server) handleIngestion(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	org, err := policyOrg(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	runs, err := s.cfg.Accounts.St.ListIngestRuns(ctx, org, r.PathValue("repo"), 50)
	if err != nil {
		s.accountError(w, err)
		return
	}
	var latest, success *accounts.IngestRun
	if len(runs) > 0 {
		item := ingestSummary(runs[0])
		latest = &item
	}
	last, err := s.cfg.Accounts.St.LatestSuccessfulIngestRun(ctx, org, r.PathValue("repo"))
	if err == nil {
		item := ingestSummary(last)
		success = &item
	} else if !errors.Is(err, accounts.ErrNotFound) {
		s.accountError(w, err)
		return
	}
	limit := min(max(intParam(r, "limit", 10), 1), 50)
	start := 0
	if before := r.URL.Query().Get("before"); before != "" {
		start = len(runs)
		for i, run := range runs {
			if run.Report.RunID == before {
				start = i + 1
				break
			}
		}
	}
	end := min(start+limit, len(runs))
	history := make([]accounts.IngestRun, 0, end-start)
	for _, run := range runs[start:end] {
		history = append(history, ingestSummary(run))
	}
	next := ""
	if end < len(runs) && end > start {
		next = runs[end-1].Report.RunID
	}
	var metrics *ingestreport.Report
	if success != nil {
		metrics = &success.Report
	}
	writeJSON(w, 200, map[string]any{"latest_attempt": latest, "last_successful": success, "metrics": metrics, "history": history, "next": next,
		"annotation_notice": "Annotation jobs are separate from file accounting; see Indexing policy for current results."})
}

func (s *Server) handleIngestRun(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	org, err := policyOrg(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	run, err := s.cfg.Accounts.St.GetIngestRun(ctx, org, r.PathValue("repo"), r.PathValue("run"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	start := min(max(intParam(r, "offset", 0), 0), len(run.Report.Files))
	end := min(start+min(max(intParam(r, "limit", 100), 1), 500), len(run.Report.Files))
	files := run.Report.Files[start:end]
	next := 0
	if end < len(run.Report.Files) {
		next = end
	}
	writeJSON(w, 200, map[string]any{"run": ingestSummary(run), "files": files, "next_offset": next})
}
