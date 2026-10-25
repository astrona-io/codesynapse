package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/forge"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
)

// Allowed auto-sync intervals in minutes. A short list keeps the UI simple
// and stops anyone from hammering a git host.
var allowedIntervals = []int{0, 5, 15, 30, 60, 360, 1440}

// defaultInterval is the auto-sync interval of a repository nobody has
// configured: new repositories stay fresh without a visit to the settings.
const defaultInterval = 30

// Schedule is the auto-sync setting of one repository.
type Schedule struct {
	Execution execution.Identity `json:"execution"`
	Repo      string             `json:"repo"`
	OrgID     string             `json:"org_id,omitempty"`
	ActorID   string             `json:"actor_id,omitempty"`
	// IntervalMinutes is 0 for manual-only.
	IntervalMinutes int  `json:"interval_minutes"`
	Paused          bool `json:"paused"`
	// AutoReview asks the AI to review commits that arrive with a sync.
	AutoReview bool `json:"auto_review"`
	// AutoTriage asks the AI to triage security findings new since the previous sync.
	AutoTriage bool   `json:"auto_triage"`
	LastRun    string `json:"last_run,omitempty"`
	LastStatus string `json:"last_status,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	// Missing counts consecutive scheduled syncs that failed while the provider
	// confirmed the repository no longer exists.
	Missing int    `json:"missing,omitempty"`
	NextRun string `json:"next_run,omitempty"`
}

// State is "manual", "paused" or "active".
func (s Schedule) State() string {
	switch {
	case s.IntervalMinutes == 0:
		return "manual"
	case s.Paused:
		return "paused"
	}
	return "active"
}

// MarshalJSON adds the derived state.
func (s Schedule) MarshalJSON() ([]byte, error) {
	type plain Schedule
	return json.Marshal(struct {
		plain
		State string `json:"state"`
	}{plain(s), s.State()})
}

// schedules persists auto-sync settings outside the graph, because every
// ingest replaces a repository's graph. With accounts they live in the
// accounts store, shared by every API and worker process; without, in a
// JSON file in the API's workspace.
type schedules struct {
	mu   sync.Mutex
	path string
	all  map[string]*Schedule
	// db, when set, holds every schedule; all and path are unused.
	db     accounts.Store
	logger *slog.Logger
}

// useStore keeps schedules in st. When importFile is set, a schedules file
// from before moves in once (existing rows win) and is renamed, so it is
// never read again.
func (s *schedules) useStore(ctx context.Context, st accounts.Store, importFile bool, logger *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db, s.logger = st, logger
	if !importFile || len(s.all) == 0 || s.path == "" {
		return
	}
	for _, sc := range s.all {
		if err := st.EnsureSchedule(ctx, scheduleRecord(*sc)); err != nil {
			logger.Error("schedules file not moved into the database; it stays in place", "error", err)
			return
		}
	}
	if err := os.Rename(s.path, s.path+".imported"); err != nil {
		logger.Warn("schedules moved into the database; renaming the file failed", "error", err)
		return
	}
	logger.Info("schedules moved into the database", "schedules", len(s.all))
}

func scheduleRecord(sc Schedule) accounts.Schedule {
	r := accounts.Schedule{Repo: sc.Repo, OrgID: sc.OrgID, ActorID: sc.ActorID, Execution: sc.Execution, IntervalMinutes: sc.IntervalMinutes,
		Paused: sc.Paused, AutoReview: sc.AutoReview, AutoTriage: sc.AutoTriage, LastStatus: sc.LastStatus, LastError: sc.LastError, Missing: sc.Missing}
	r.LastRun, _ = time.Parse(time.RFC3339, sc.LastRun) //nolint:errcheck // empty or unreadable means never
	r.NextRun, _ = time.Parse(time.RFC3339, sc.NextRun) //nolint:errcheck // as above
	return r
}

func scheduleView(r accounts.Schedule) Schedule {
	sc := Schedule{Repo: r.Repo, OrgID: r.OrgID, ActorID: r.ActorID, Execution: r.Execution, IntervalMinutes: r.IntervalMinutes,
		Paused: r.Paused, AutoReview: r.AutoReview, AutoTriage: r.AutoTriage, LastStatus: r.LastStatus, LastError: r.LastError, Missing: r.Missing}
	if !r.LastRun.IsZero() {
		sc.LastRun = r.LastRun.UTC().Format(time.RFC3339)
	}
	if !r.NextRun.IsZero() {
		sc.NextRun = r.NextRun.UTC().Format(time.RFC3339)
	}
	return sc
}

// dbCtx bounds one store call.
func dbCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 10*time.Second)
}

func (s *schedules) warn(what string, err error) {
	if err != nil && s.logger != nil {
		s.logger.Warn(what, "error", err)
	}
}

func loadSchedules(dir string) (*schedules, error) {
	s := &schedules{all: map[string]*Schedule{}}
	if dir == "" {
		return s, nil
	}
	s.path = filepath.Join(dir, ".codesynapse", "schedules.json")
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var list []*Schedule
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	for _, sc := range list {
		s.all[sc.Repo] = sc
	}
	return s, nil
}

// save writes atomically. Callers hold mu.
func (s *schedules) save() error {
	if s.path == "" {
		return nil
	}
	list := make([]*Schedule, 0, len(s.all))
	for _, sc := range s.all {
		list = append(list, sc)
	}
	slices.SortFunc(list, func(a, b *Schedule) int {
		if a.Repo < b.Repo {
			return -1
		}
		return 1
	})
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *schedules) get(ctx context.Context, repo string) Schedule {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		r, err := s.db.ScheduleByRepo(ctx, repo)
		if err == nil {
			return scheduleView(r)
		}
		if !errors.Is(err, accounts.ErrNotFound) {
			s.warn("reading a schedule", err)
		}
		return Schedule{Repo: repo, IntervalMinutes: defaultInterval}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc, ok := s.all[repo]; ok {
		return *sc
	}
	return Schedule{Repo: repo, IntervalMinutes: defaultInterval}
}

// stagger spreads first runs over one interval, derived from the repository
// name so it is stable across restarts. Without it every repository added
// together (an organisation scan) would be due in the same minute, again and
// again.
func stagger(repo string, interval int) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(repo)) //nolint:errcheck // hash writes never fail
	return time.Duration(int64(h.Sum32())%int64(interval*60)) * time.Second
}

// ensure stores the default schedule for repo when it has none, so the
// scheduler has a next run to count down to.
func (s *schedules) ensure(ctx context.Context, repo string, now time.Time) {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		s.warn("adding a default schedule", s.db.EnsureSchedule(ctx, accounts.Schedule{Repo: repo, IntervalMinutes: defaultInterval,
			NextRun: now.Add(stagger(repo, defaultInterval)).UTC().Truncate(time.Second)}))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.all[repo]; ok {
		return
	}
	s.all[repo] = &Schedule{Repo: repo, IntervalMinutes: defaultInterval,
		NextRun: now.Add(stagger(repo, defaultInterval)).UTC().Format(time.RFC3339)}
	_ = s.save() //nolint:errcheck // best effort; the next tick retries
}

func (s *schedules) set(ctx context.Context, repo string, interval int, paused, autoReview, autoTriage bool, now time.Time, delegate ...Schedule) (Schedule, error) {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		current := Schedule{Repo: repo}
		r, err := s.db.ScheduleByRepo(ctx, repo)
		switch {
		case err == nil:
			current = scheduleView(r)
		case !errors.Is(err, accounts.ErrNotFound):
			return Schedule{}, err
		}
		next, err := applySettings(&current, interval, paused, autoReview, autoTriage, now, delegate...)
		if err != nil {
			return Schedule{}, err
		}
		return next, s.db.PutSchedule(ctx, scheduleRecord(next))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[repo]
	if !ok {
		sc = &Schedule{Repo: repo}
	}
	next, err := applySettings(sc, interval, paused, autoReview, autoTriage, now, delegate...)
	if err != nil {
		return Schedule{}, err
	}
	*sc = next
	s.all[repo] = sc
	return next, s.save()
}

// applySettings is sc with new settings (and delegated identity) applied.
func applySettings(cur *Schedule, interval int, paused, autoReview, autoTriage bool, now time.Time, delegate ...Schedule) (Schedule, error) {
	sc := *cur
	if len(delegate) > 0 {
		if sc.OrgID != "" && sc.OrgID != delegate[0].OrgID {
			return Schedule{}, accounts.ErrForbidden
		}
		sc.OrgID, sc.ActorID = delegate[0].OrgID, delegate[0].ActorID
		sc.Execution = delegate[0].Execution
	}
	sc.IntervalMinutes, sc.Paused, sc.AutoReview, sc.AutoTriage = interval, paused, autoReview, autoTriage
	sc.NextRun = ""
	if sc.State() == "active" {
		sc.NextRun = now.Add(time.Duration(interval) * time.Minute).UTC().Format(time.RFC3339)
	}
	return sc, nil
}

func (s *schedules) remove(ctx context.Context, repo string) error {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		return s.db.DeleteSchedule(ctx, repo)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.all, repo)
	return s.save()
}

// due returns active schedules whose next run has passed.
func (s *schedules) due(ctx context.Context, now time.Time) []string {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		repos, err := s.db.DueSchedules(ctx, now)
		s.warn("listing due schedules", err)
		return repos
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for repo, sc := range s.all {
		if sc.State() != "active" {
			continue
		}
		next, err := time.Parse(time.RFC3339, sc.NextRun)
		if err != nil || !now.Before(next) {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

// started records a run (status: started, or unchanged when nothing was new)
// and schedules the next one.
func (s *schedules) started(ctx context.Context, repo string, now time.Time, status string, err error) {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		run := accounts.ScheduleRun{LastRun: now.UTC().Truncate(time.Second), LastStatus: status, ResetMissing: err == nil}
		if err != nil {
			run.LastStatus, run.LastError = "failed", err.Error()
		}
		sc := s.get(ctx, repo)
		run.NextRun = now.Add(time.Duration(sc.IntervalMinutes) * time.Minute).UTC().Truncate(time.Second)
		s.warn("recording a scheduled run", s.db.RecordScheduleRun(ctx, repo, run))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[repo]
	if !ok {
		return
	}
	sc.LastRun = now.UTC().Format(time.RFC3339)
	sc.LastStatus, sc.LastError = status, ""
	if err != nil {
		sc.LastStatus, sc.LastError = "failed", err.Error()
	} else {
		sc.Missing = 0
	}
	sc.NextRun = now.Add(time.Duration(sc.IntervalMinutes) * time.Minute).UTC().Format(time.RFC3339)
	_ = s.save() //nolint:errcheck // best effort; the next tick retries
}

// finished schedules the next run one interval after a scheduled sync ended,
// so a long sync never eats into the gap and repositories drift apart.
func (s *schedules) finished(ctx context.Context, repo string, now time.Time, err error) {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		run := accounts.ScheduleRun{LastStatus: "succeeded"}
		if err != nil {
			run.LastStatus, run.LastError = "failed", err.Error()
		}
		sc := s.get(ctx, repo)
		run.NextRun = now.Add(time.Duration(sc.IntervalMinutes) * time.Minute).UTC().Truncate(time.Second)
		s.warn("recording a scheduled run", s.db.RecordScheduleRun(ctx, repo, run))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[repo]
	if !ok {
		return
	}
	sc.LastStatus, sc.LastError = "succeeded", ""
	if err != nil {
		sc.LastStatus, sc.LastError = "failed", err.Error()
	}
	if sc.State() == "active" {
		sc.NextRun = now.Add(time.Duration(sc.IntervalMinutes) * time.Minute).UTC().Format(time.RFC3339)
	}
	_ = s.save() //nolint:errcheck // best effort; the next tick retries
}

// setMissing bumps (gone) or clears the count of consecutive confirmed-missing
// checks and returns the new count.
func (s *schedules) setMissing(ctx context.Context, repo string, gone bool) int {
	if s.db != nil {
		ctx, cancel := dbCtx(ctx)
		defer cancel()
		n, err := s.db.AddScheduleMissing(ctx, repo, gone)
		s.warn("counting a missing repository", err)
		return n
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[repo]
	if !ok {
		return 0
	}
	if gone {
		sc.Missing++
	} else {
		sc.Missing = 0
	}
	_ = s.save() //nolint:errcheck // best effort; the next tick retries
	return sc.Missing
}

// goneStrikes is how many scheduled syncs in a row must find the repository
// missing at its provider before the clone and graph are removed.
const goneStrikes = 3

// reapIfGone removes a cloned repository the provider says no longer exists.
// A failed sync alone proves nothing (network, rate limit, lost access), so
// it needs the provider API to answer a clean 404 goneStrikes times in a row;
// any other answer resets the count. Mounted sources are never touched.
func (s *Server) reapIfGone(ctx context.Context, repo string, logger *slog.Logger) {
	if _, err := execution.Require(ctx); err != nil {
		return
	}
	if s.cfg.Accounts != nil {
		return
	}
	n, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil || !s.inWorkspace(n.Root) {
		return
	}
	u, err := url.Parse(n.URL)
	if err != nil {
		return
	}
	host := strings.ToLower(u.Hostname())
	parts := strings.Split(strings.Trim(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
	if len(parts) != 2 || u.Port() != "" || !forge.ValidHost(host) {
		return
	}
	kind, ok := forge.KindForHost(host)
	for _, o := range s.orgs.list() {
		if !ok && o.Host == host {
			kind, ok = o.Provider, true
		}
	}
	if !ok {
		return // a host whose provider is unknown cannot be asked
	}
	client, err := s.forgeClient(ctx, Org{Provider: kind, Host: host, Org: parts[0]})
	if err != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	exists, err := client.RepoExists(cctx, parts[0], parts[1])
	if err != nil {
		return
	}
	count := s.sched.setMissing(ctx, repo, !exists)
	if exists || count < goneStrikes {
		if !exists {
			logger.Warn("repository not found at its provider", "repo", repo, "checks", count, "of", goneStrikes)
		}
		return
	}
	if _, err := s.deleteRepo(ctx, repo, true); err != nil {
		logger.Warn("removing repository that no longer exists", "repo", repo, "error", err)
		return
	}
	logger.Info("removed repository that no longer exists at its provider", "repo", repo, "remote", gitx.SanitizeURL(n.URL))
}

// runScheduler starts due syncs every minute until ctx ends.
func (s *Server) runScheduler(ctx context.Context, logger *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if repos, err := s.st.Repos(ctx); err != nil {
				logger.Warn("listing repositories for auto-sync", "error", err)
			} else {
				for _, n := range repos {
					s.sched.ensure(ctx, n.Name, now)
				}
			}
			if s.cfg.Accounts == nil {
				s.scanDue(ctx, now, logger)
			} else {
				s.scanWatches(ctx, now, logger)
			}
			for _, repo := range s.sched.due(ctx, now) {
				sc := s.sched.get(ctx, repo)
				jobCtx, err := s.scheduleContext(ctx, sc)
				if err != nil {
					s.sched.started(ctx, repo, now, "failed", err)
					continue
				}
				_, err = s.startSync(jobCtx, repo, "", true, func(err error) { s.sched.finished(context.WithoutCancel(ctx), repo, time.Now(), err) })
				switch {
				case errors.Is(err, errConflict):
					continue // a sync is already running; try again next interval
				case errors.Is(err, errUnchanged):
					s.sched.started(ctx, repo, now, "unchanged", nil)
				case err != nil:
					logger.Warn("scheduled sync not started", "repo", repo, "error", err)
					s.sched.started(ctx, repo, now, "failed", err)
					s.reapIfGone(jobCtx, repo, logger)
				default:
					s.sched.started(ctx, repo, now, "started", nil)
				}
			}
		}
	}
}

func (s *Server) scheduleContext(ctx context.Context, sc Schedule) (context.Context, error) {
	if s.cfg.Accounts == nil {
		return nil, errExecutionRegistry
	}
	ctx, err := execution.With(ctx, sc.Execution)
	if err != nil {
		return nil, err
	}
	if sc.OrgID == "" || sc.ActorID == "" || sc.Execution.SubjectID != sc.ActorID || sc.Execution.TenantID != sc.OrgID {
		return nil, execution.ErrConflict
	}
	if _, err := s.cfg.Accounts.ResolveExecutionSession(ctx, sc.ActorID, sc.OrgID, sc.Execution.SessionID); err != nil {
		return nil, err
	}
	p := accounts.Principal{User: accounts.User{ID: sc.ActorID}, OrgID: sc.OrgID, Via: "automation"}
	access, err := s.cfg.Accounts.ResolveRepository(ctx, p, sc.OrgID, sc.Repo, accounts.Admin)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, principalKey{}, p)
	ctx = context.WithValue(ctx, scopeKey{}, scope{graphs: map[string]accounts.Role{sc.Repo: access.Role}, reach: map[string]accounts.Reach{sc.Repo: {OrgID: sc.OrgID, Role: access.Role}}})
	return ctx, nil
}

type scheduleRequest struct {
	IntervalMinutes int  `json:"interval_minutes"`
	Paused          bool `json:"paused"`
	AutoReview      bool `json:"auto_review"`
	AutoTriage      bool `json:"auto_triage"`
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	sc := s.sched.get(r.Context(), r.PathValue("repo"))
	if s.cfg.Accounts != nil {
		org, err := s.repoOrg(r, sc.Repo)
		if err != nil {
			s.accountError(w, err)
			return
		}
		if sc.OrgID != org {
			sc = Schedule{Repo: sc.Repo}
		}
	}
	writeJSON(w, 200, sc)
}

func (s *Server) handlePutSchedule(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if _, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo)); err != nil {
		s.problem(w, 500, err)
		return
	}
	var req scheduleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if !slices.Contains(allowedIntervals, req.IntervalMinutes) {
		s.problem(w, 400, fmt.Errorf("interval_minutes must be one of %v", allowedIntervals))
		return
	}
	delegate := Schedule{Execution: identityOf(r.Context())}
	if (req.IntervalMinutes > 0 && !req.Paused) || req.AutoReview || req.AutoTriage {
		if s.cfg.Accounts == nil {
			s.problem(w, http.StatusForbidden, errExecutionRegistry)
			return
		}
		if _, err := execution.Require(r.Context()); err != nil {
			s.problem(w, http.StatusForbidden, err)
			return
		}
	}
	if s.cfg.Accounts != nil {
		org, err := s.repoOrg(r, repo)
		if err != nil {
			s.accountError(w, err)
			return
		}
		p, _ := principal(r.Context())
		if p.Via == "key" {
			s.accountError(w, accounts.ErrForbidden)
			return
		}
		delegate.OrgID, delegate.ActorID = org, p.User.ID
	}
	sc, err := s.sched.set(r.Context(), repo, req.IntervalMinutes, req.Paused, req.AutoReview, req.AutoTriage, time.Now(), delegate)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, sc)
}
