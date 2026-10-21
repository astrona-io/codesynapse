package api

import (
	"errors"
	"net/http"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Dashboard bundles everything the repository landing page shows, so the UI
// renders with one request.
type Dashboard struct {
	Repo          query.Ref       `json:"repo"`
	Head          string          `json:"head,omitempty"`
	IngestedAt    string          `json:"ingested_at,omitempty"`
	Stats         model.Stats     `json:"stats"`
	Hotspots      []query.Hotspot `json:"hotspots"`
	MostCalled    []store.Ranked  `json:"most_called"`
	ChurnFiles    []store.Ranked  `json:"churn_files"`
	LargestFiles  []store.Ranked  `json:"largest_files"`
	Undocumented  []store.Ranked  `json:"undocumented"`
	Security      []store.Ranked  `json:"security"`
	RecentCommits []store.Ranked  `json:"recent_commits"`
	Authors       []store.Ranked  `json:"authors"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo := r.PathValue("repo")
	st, err := s.st.Stats(ctx, repo)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	d := Dashboard{Stats: st}
	if n, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo)); err == nil {
		d.Repo, d.Head, d.IngestedAt = query.RefOf(n), n.Head, n.Time
	}
	callables := []model.NodeKind{model.KindFunction, model.KindMethod}

	if d.Hotspots, err = s.svc.Hotspots(ctx, repo, 10); err != nil {
		s.problem(w, 500, err)
		return
	}
	lists := []struct {
		dst    *[]store.Ranked
		filter store.Filter
		metric store.Metric
		limit  int
	}{
		{&d.MostCalled, store.Filter{Kinds: callables, ExcludeTests: true}, store.ByFanIn, 10},
		{&d.ChurnFiles, store.Filter{Kinds: []model.NodeKind{model.KindFile}}, store.ByChurn, 10},
		{&d.LargestFiles, store.Filter{Kinds: []model.NodeKind{model.KindFile}, ExcludeTests: true}, store.ByLOC, 10},
		{&d.Undocumented, store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod, model.KindType}, ExportedOnly: true, Undocumented: true, ExcludeTests: true}, store.ByFanIn, 10},
		{&d.RecentCommits, store.Filter{Kinds: []model.NodeKind{model.KindCommit}}, store.ByTime, 15},
		{&d.Authors, store.Filter{Kinds: []model.NodeKind{model.KindAuthor}}, store.ByAuthored, 10},
	}
	for _, l := range lists {
		res, err := s.st.List(ctx, repo, l.filter, l.metric, l.limit)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		*l.dst = nonNilRanked(res)
	}
	sec, err := s.st.List(ctx, repo, store.Filter{Kinds: callables, ExcludeTests: true}, store.ByFanIn, store.MaxLimit)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	d.Security = []store.Ranked{}
	for _, r := range sec {
		if len(r.Node.SecurityTags) > 0 && len(d.Security) < 15 {
			d.Security = append(d.Security, r)
		}
	}
	if d.Hotspots == nil {
		d.Hotspots = []query.Hotspot{}
	}
	writeJSON(w, 200, d)
}

func nonNilRanked(r []store.Ranked) []store.Ranked {
	if r == nil {
		return []store.Ranked{}
	}
	return r
}

// handleMetrics serves the metrics report computed at ingest time.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	n, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if n.Metrics == "" {
		s.problem(w, http.StatusNotFound, errors.New("no metrics stored; re-ingest the repository"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(n.Metrics)) //nolint:errcheck // client went away
}

// handleCleanup serves the unused-code report computed at ingest time.
func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	n, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if n.Cleanup == "" {
		s.problem(w, http.StatusNotFound, errors.New("no cleanup report stored; re-ingest (Sync) the repository"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(n.Cleanup)) //nolint:errcheck // client went away
}
