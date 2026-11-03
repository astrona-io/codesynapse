package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/authz/fgasync"
)

// OpenFGA sync (docs/plans/OPENFGA_PLAN.md, phase 5). PostgreSQL stays the
// source of truth; this loop keeps OpenFGA's tuples in step, and with
// CODESYNAPSE_AUTHZ=openfga every API process asks OpenFGA (phase 6). It
// runs under the api-loops lease, so one process writes.
const (
	// openFGAStore is the store the API creates or selects.
	openFGAStore = "codesynapse"
	// openFGAEvery is how often pending organisations are synced. An
	// organisation is refused (stale) from a policy change until its sync,
	// so this is short.
	openFGAEvery = 2 * time.Second
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

// openFGADecider asks OpenFGA for every decision (phase 6). It only reads:
// the store and model are the ones the sync loop wrote, so a process that
// does not hold the lease never creates either. Until they exist, or when
// the records cannot be read, every decision is refused.
func (s *Server) openFGADecider() accounts.Decider {
	records, recErr := s.cfg.Accounts.OpenFGARecords()
	client, clientErr := fga.New(*s.cfg.OpenFGA)
	if err := errors.Join(recErr, clientErr); err != nil {
		s.logger.Error("openfga decisions refused", "error", err)
		return refuseDecider{err: err}
	}
	return &fgasync.Decider{Records: records, Select: func(ctx context.Context) (fgasync.Checker, error) {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		c, err := client.Select(sctx, openFGAStore)
		if err != nil {
			return nil, err // never a nil *fga.Client inside the interface
		}
		return c, nil
	}}
}

// refuseDecider refuses every decision.
type refuseDecider struct{ err error }

func (r refuseDecider) Decide(context.Context, accounts.DecisionRequest) (accounts.Decision, error) {
	return accounts.Decision{}, r.err
}

// refuseAccess answers no access question: a worker with accounts but no
// API to ask (the command refuses to start that way).
type refuseAccess struct{}

func (refuseAccess) AccessIn(context.Context, accounts.Principal, accounts.AccessQuery) (accounts.Access, error) {
	return accounts.Access{}, fmt.Errorf("%w: this worker has no API to ask about access (CODESYNAPSE_API_URL)", accounts.ErrForbidden)
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
