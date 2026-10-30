package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// The developer's failover proxy (`codesynapse gateway proxy`) sends a
// request straight to Anthropic only while the gateway cannot be reached.
// Those requests skip the gateway and its audit, so the proxy reports them
// here once the gateway answers again: when each outage began and ended, how
// many requests went direct, and by which model. Nothing else is sent.
const (
	failoverPath       = "/v1/codesynapse/failover"
	maxFailoverWindows = 32
	maxFailoverModels  = 8
	maxFailoverCount   = 1_000_000_000
	// maxFailoverAge refuses windows from long ago (a clock far off, or a file
	// copied around); the proxy drops counts the gateway refuses.
	maxFailoverAge = 400 * 24 * time.Hour
	failoverSkew   = 10 * time.Minute
)

type failoverWindow struct {
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end"`
	Requests int            `json:"requests"`
	Models   map[string]int `json:"models,omitempty"`
}

type failoverReport struct {
	Windows []failoverWindow `json:"windows"`
}

func (r failoverReport) validate(now time.Time) error {
	if len(r.Windows) == 0 || len(r.Windows) > maxFailoverWindows {
		return fmt.Errorf("between 1 and %d windows", maxFailoverWindows)
	}
	for _, w := range r.Windows {
		switch {
		case w.Start.IsZero() || w.End.Before(w.Start):
			return errors.New("a window ends before it starts")
		case w.End.After(now.Add(failoverSkew)) || w.Start.Before(now.Add(-maxFailoverAge)):
			return errors.New("a window lies outside the accepted time range")
		case w.Requests < 0 || w.Requests > maxFailoverCount:
			return errors.New("a window's request count is out of range")
		case len(w.Models) > maxFailoverModels:
			return fmt.Errorf("at most %d models per window", maxFailoverModels)
		}
		for _, n := range w.Models {
			if n < 0 || n > maxFailoverCount {
				return errors.New("a model count is out of range")
			}
		}
	}
	return nil
}

// failoverDetail is the audit detail of one window: times, counts and plain
// model names only (anything else counts as "other").
func failoverDetail(w failoverWindow) string {
	counts := map[string]int{}
	for m, n := range w.Models {
		if !modelName.MatchString(m) {
			m = "other"
		}
		counts[m] = min(counts[m]+n, maxFailoverCount)
	}
	names := make([]string, 0, len(counts))
	for m := range counts {
		names = append(names, m)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, m := range names {
		parts = append(parts, fmt.Sprintf("%s:%d", m, counts[m]))
	}
	d := fmt.Sprintf("start=%s end=%s requests=%d", w.Start.UTC().Format(time.RFC3339), w.End.UTC().Format(time.RFC3339), w.Requests)
	if len(parts) > 0 {
		d += " models=" + strings.Join(parts, ",")
	}
	return d
}

// handleGatewayFailover records each reported window as a gateway.failover
// audit event. The proxy keeps the counts until this answers 2xx, so a
// failed audit write answers 503 and the report comes again later.
func (s *Server) handleGatewayFailover(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	m, err := scopeFrom(r.Context()).selected()
	if err != nil {
		s.problem(w, http.StatusForbidden, errors.New("the gateway needs one organisation"))
		return
	}
	var rep failoverReport
	if err := decodeJSON(w, r, &rep); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := rep.validate(time.Now()); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := auditContext(r.Context())
	defer cancel()
	total := 0
	for _, win := range rep.Windows {
		e := accounts.Event{
			Action: "gateway.failover", Target: failoverPath, Status: http.StatusOK, Outcome: accounts.Success,
			Detail: failoverDetail(win), UserID: p.User.ID, ActorEmail: p.User.Email, Via: p.Via, KeyID: p.KeyID, OrgID: m.Org.ID,
		}
		if _, err := s.cfg.Accounts.Record(ctx, e); err != nil {
			s.logger.Error("audit log write failed", "action", e.Action, "error", err)
			s.problem(w, http.StatusServiceUnavailable, errors.New("the failover report could not be recorded; send it again later"))
			return
		}
		total += win.Requests
	}
	telemetry.GatewayFailover.AddContext(ctx, float64(total), telemetry.Tenant(ctx))
	w.WriteHeader(http.StatusNoContent)
}
