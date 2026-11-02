package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/authz/fgasync"
)

// OpenFGA sync (docs/plans/OPENFGA_PLAN.md, phase 5). PostgreSQL decides
// every access; this loop only keeps OpenFGA's tuples in step so phase 6
// can ask it. It runs under the api-loops lease, so one process writes.
const (
	// openFGAStore is the store the API creates or selects.
	openFGAStore = "codesynapse"
	// openFGAEvery is how often pending organisations are synced.
	openFGAEvery = 15 * time.Second
	// openFGARun bounds one pass; well under the lease's minute.
	openFGARun = 40 * time.Second
	// openFGABatch is how many organisations one pass syncs.
	openFGABatch = 50
	// openFGASetupMax caps the wait between attempts to reach OpenFGA.
	openFGASetupMax = 5 * time.Minute
)

func (s *Server) syncOpenFGA(ctx context.Context) {
	records, err := s.cfg.Accounts.OpenFGARecords()
	if err != nil {
		s.logger.Error("openfga sync off", "error", err)
		return
	}
	client, err := fga.New(*s.cfg.OpenFGA)
	if err != nil {
		s.logger.Error("openfga sync off", "error", err)
		return
	}
	// Select (or create) the store and model, retrying while OpenFGA starts.
	for wait := 5 * time.Second; ; wait = min(2*wait, openFGASetupMax) {
		err = setupOpenFGA(ctx, client)
		if err == nil {
			break
		}
		s.logger.Warn("openfga not ready; retrying", "error", err, "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
	// A new leader compares every organisation once: drift from a lost
	// write, an earlier leader or a change made by hand gets repaired.
	if _, err := records.RequestOpenFGARebuild(ctx, "", time.Now()); err != nil && ctx.Err() == nil {
		s.logger.Warn("openfga rebuild not requested", "error", err)
	}
	syncer := &fgasync.Syncer{Client: client, Records: records, Policy: s.cfg.Accounts.OrgPolicy}
	ticker := time.NewTicker(openFGAEvery)
	defer ticker.Stop()
	for {
		run, cancel := context.WithTimeout(ctx, openFGARun)
		_, err := syncer.SyncPending(run, openFGABatch)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("openfga sync deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func setupOpenFGA(ctx context.Context, c *fga.Client) error {
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.EnsureStore(sctx, openFGAStore); err != nil {
		return err
	}
	_, err := c.EnsureModel(sctx)
	return err
}

var errOpenFGAOff = errors.New("OpenFGA is not configured")

// handleOpenFGARebuild asks the sync loop to compare one organisation
// (?org=ID) or every one with OpenFGA and repair the drift. Platform
// admins in a signed-in session; audited before it is stored.
func (s *Server) handleOpenFGARebuild(w http.ResponseWriter, r *http.Request) {
	if !s.platformAdmin(w, r) {
		return
	}
	if s.cfg.OpenFGA == nil {
		s.problem(w, http.StatusConflict, errOpenFGAOff)
		return
	}
	p, _ := principal(r.Context())
	n, err := s.cfg.Accounts.RequestOpenFGARebuild(r.Context(), p, r.URL.Query().Get("org"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"organisations": n})
}
