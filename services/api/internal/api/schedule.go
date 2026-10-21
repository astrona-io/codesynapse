package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// Allowed auto-sync intervals in minutes. A short list keeps the UI simple
// and stops anyone from hammering a git host.
var allowedIntervals = []int{0, 5, 15, 60, 360, 1440}

// defaultInterval is the auto-sync interval of a repository nobody has
// configured: new repositories stay fresh without a visit to the settings.
const defaultInterval = 5

// Schedule is the auto-sync setting of one repository.
type Schedule struct {
	Repo string `json:"repo"`
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
	NextRun    string `json:"next_run,omitempty"`
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

// schedules persists auto-sync settings in a JSON file in the API's
// writable state directory (the workspace volume). They live outside the
// graph because every ingest replaces a repository's graph.
type schedules struct {
	mu   sync.Mutex
	path string
	all  map[string]*Schedule
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

func (s *schedules) get(repo string) Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc, ok := s.all[repo]; ok {
		return *sc
	}
	return Schedule{Repo: repo, IntervalMinutes: defaultInterval}
}

// ensure stores the default schedule for repo when it has none, so the
// scheduler has a next run to count down to.
func (s *schedules) ensure(repo string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.all[repo]; ok {
		return
	}
	s.all[repo] = &Schedule{Repo: repo, IntervalMinutes: defaultInterval,
		NextRun: now.Add(defaultInterval * time.Minute).UTC().Format(time.RFC3339)}
	_ = s.save() //nolint:errcheck // best effort; the next tick retries
}

func (s *schedules) set(repo string, interval int, paused, autoReview, autoTriage bool, now time.Time) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[repo]
	if !ok {
		sc = &Schedule{Repo: repo}
		s.all[repo] = sc
	}
	sc.IntervalMinutes, sc.Paused, sc.AutoReview, sc.AutoTriage = interval, paused, autoReview, autoTriage
	sc.NextRun = ""
	if sc.State() == "active" {
		sc.NextRun = now.Add(time.Duration(interval) * time.Minute).UTC().Format(time.RFC3339)
	}
	return *sc, s.save()
}

func (s *schedules) remove(repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.all, repo)
	return s.save()
}

// due returns active schedules whose next run has passed.
func (s *schedules) due(now time.Time) []string {
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

// started records a run and schedules the next one.
func (s *schedules) started(repo string, now time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sc, ok := s.all[repo]
	if !ok {
		return
	}
	sc.LastRun = now.UTC().Format(time.RFC3339)
	sc.LastStatus, sc.LastError = "started", ""
	if err != nil {
		sc.LastStatus, sc.LastError = "failed", err.Error()
	}
	sc.NextRun = now.Add(time.Duration(sc.IntervalMinutes) * time.Minute).UTC().Format(time.RFC3339)
	_ = s.save() //nolint:errcheck // best effort; the next tick retries
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
					s.sched.ensure(n.Name, now)
				}
			}
			for _, repo := range s.sched.due(now) {
				_, err := s.startSync(ctx, repo, "")
				if errors.Is(err, errConflict) {
					continue // a sync is already running; try again next interval
				}
				if err != nil {
					logger.Warn("scheduled sync not started", "repo", repo, "error", err)
				}
				s.sched.started(repo, now, err)
			}
		}
	}
}

type scheduleRequest struct {
	IntervalMinutes int  `json:"interval_minutes"`
	Paused          bool `json:"paused"`
	AutoReview      bool `json:"auto_review"`
	AutoTriage      bool `json:"auto_triage"`
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.sched.get(r.PathValue("repo")))
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
	sc, err := s.sched.set(repo, req.IntervalMinutes, req.Paused, req.AutoReview, req.AutoTriage, time.Now())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, sc)
}
