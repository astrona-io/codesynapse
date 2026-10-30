package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
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
	// maxFailoverRequests is more than a busy developer sends in days of
	// outage; the proxy saturates its counts at the same number.
	maxFailoverRequests = 100_000
	// maxFailoverAge refuses windows from long ago (a clock far off, or a file
	// copied around); the proxy does not send older ones.
	maxFailoverAge = 30 * 24 * time.Hour
	failoverSkew   = 10 * time.Minute
	// failoverEvery is how often one key may have a report recorded; the
	// proxy reports at most once a minute.
	failoverEvery = 50 * time.Second
	// maxFailoverScan bounds the look-up of windows already recorded.
	maxFailoverScan = 1000
	// maxFailoverKeys bounds the rate limiter's memory.
	maxFailoverKeys = 10_000
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
		case w.Requests < 0 || w.Requests > maxFailoverRequests:
			return errors.New("a window's request count is out of range")
		case len(w.Models) > maxFailoverModels:
			return fmt.Errorf("at most %d models per window", maxFailoverModels)
		}
		for _, n := range w.Models {
			if n < 0 || n > maxFailoverRequests {
				return errors.New("a model count is out of range")
			}
		}
	}
	return nil
}

// failoverStart is how a window is named in its audit detail, and so the key
// that keeps a window from being recorded twice.
func failoverStart(t time.Time) string { return "start=" + t.UTC().Format(time.RFC3339Nano) + " " }

// failoverDetail is the audit detail of one window: times, counts and plain
// model names only (anything else counts as "other").
func failoverDetail(w failoverWindow) string {
	counts := map[string]int{}
	for m, n := range w.Models {
		if !modelName.MatchString(m) {
			m = "other"
		}
		counts[m] = min(counts[m]+n, maxFailoverRequests)
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
	d := fmt.Sprintf("%send=%s requests=%d", failoverStart(w.Start), w.End.UTC().Format(time.RFC3339Nano), w.Requests)
	if len(parts) > 0 {
		d += " models=" + strings.Join(parts, ",")
	}
	return d
}

// failoverLimit lets each key have one report recorded per failoverEvery.
// It is per replica; with several API replicas a key gets one per replica,
// still a handful a minute.
type failoverLimit struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// take reserves the key's turn; ok is false when it had one too recently.
func (l *failoverLimit) take(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if t, ok := l.last[key]; ok && now.Sub(t) < failoverEvery {
		return false
	}
	if len(l.last) >= maxFailoverKeys {
		for k, t := range l.last {
			if now.Sub(t) >= failoverEvery {
				delete(l.last, k)
			}
		}
		if len(l.last) >= maxFailoverKeys {
			return false
		}
	}
	l.last[key] = now
	return true
}

// forget gives the key its turn back: a report that could not be recorded
// does not count.
func (l *failoverLimit) forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.last, key)
}

// handleGatewayFailover records each reported window as a gateway.failover
// audit event, once: a window this key already reported (a retry after a
// partly written report or a lost answer) is skipped. The proxy keeps the
// counts until this answers 2xx, so a failed audit write answers 503 and the
// report comes again later.
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
	on, err := s.cfg.Accounts.GatewayEnabled(r.Context(), m.Org.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	if !on {
		s.problem(w, http.StatusForbidden, errors.New("the gateway is switched off for this organisation; an owner or admin turns it on"))
		return
	}
	var rep failoverReport
	if err := decodeJSON(w, r, &rep); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	now := time.Now()
	if err := rep.validate(now); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	limitKey := p.KeyID
	if limitKey == "" {
		limitKey = "user:" + p.User.ID
	}
	if !s.failover.take(limitKey, now) {
		w.Header().Set("Retry-After", "60")
		s.problem(w, http.StatusTooManyRequests, errors.New("one failover report a minute; send it again later"))
		return
	}
	ctx, cancel := auditContext(r.Context())
	defer cancel()
	seen, err := s.failoversRecorded(ctx, p, m.Org.ID, rep)
	if err != nil {
		s.failover.forget(limitKey)
		s.logger.Error("audit log read failed", "action", "gateway.failover", "error", err)
		s.problem(w, http.StatusServiceUnavailable, errors.New("the failover report could not be recorded; send it again later"))
		return
	}
	total := 0
	for _, win := range rep.Windows {
		start := failoverStart(win.Start)
		if seen[start] {
			continue
		}
		e := accounts.Event{
			Action: "gateway.failover", Target: failoverPath, Status: http.StatusOK, Outcome: accounts.Success,
			Detail: failoverDetail(win), UserID: p.User.ID, ActorEmail: p.User.Email, Via: p.Via, KeyID: p.KeyID, OrgID: m.Org.ID,
		}
		if _, err := s.cfg.Accounts.Record(ctx, e); err != nil {
			s.failover.forget(limitKey)
			s.logger.Error("audit log write failed", "action", e.Action, "error", err)
			s.problem(w, http.StatusServiceUnavailable, errors.New("the failover report could not be recorded; send it again later"))
			// The windows written so far are counted; a retry skips them.
			telemetry.GatewayFailover.AddContext(ctx, float64(total), telemetry.Tenant(ctx))
			return
		}
		seen[start] = true
		total += win.Requests
	}
	telemetry.GatewayFailover.AddContext(ctx, float64(total), telemetry.Tenant(ctx))
	w.WriteHeader(http.StatusNoContent)
}

// failoversRecorded names the windows of rep this key (or, without a key,
// this user) already has in the audit log.
func (s *Server) failoversRecorded(ctx context.Context, p accounts.Principal, org string, rep failoverReport) (map[string]bool, error) {
	since := rep.Windows[0].Start
	for _, w := range rep.Windows {
		if w.Start.Before(since) {
			since = w.Start
		}
	}
	// A window is recorded after it started, by the server's clock give or
	// take the skew allowed for the developer's.
	rows, err := s.cfg.Accounts.St.ListEvents(ctx, accounts.EventFilter{
		OrgID: org, Action: "gateway.failover", Actor: p.User.ID, Since: since.Add(-failoverSkew), Limit: maxFailoverScan,
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range rows {
		if e.Action != "gateway.failover" || e.KeyID != p.KeyID || !strings.HasPrefix(e.Detail, "start=") {
			continue
		}
		if i := strings.IndexByte(e.Detail, ' '); i > 0 {
			seen[e.Detail[:i+1]] = true
		}
	}
	return seen, nil
}
