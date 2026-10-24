package api

import (
	"context"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Site search for the dashboard's top bar: repositories by name, then
// symbols, entry points, security findings and doc sections in the
// repositories the caller may see. The repository being viewed is searched
// first; the rest in name order until the limits are reached, so a large
// installation answers quickly with what it found so far.

const (
	searchMaxRepos   = 50
	searchPerRepo    = 5
	searchMaxResults = 30
	searchTimeout    = 3 * time.Second
	searchMinQuery   = 2
	searchMaxQuery   = 200
)

var siteSearchKinds = []model.NodeKind{
	model.KindFunction, model.KindMethod, model.KindInterfaceMethod, model.KindType,
	model.KindRoute, model.KindFinding, model.KindSection,
}

// SearchHit is one site search result.
type SearchHit struct {
	// Kind is "repository" or a node kind (Function, Route, Finding, ...).
	Kind     model.NodeKind `json:"kind"`
	Repo     string         `json:"repo"`
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Location string         `json:"location,omitempty"`
	File     string         `json:"file,omitempty"`
	Severity string         `json:"severity,omitempty"`
}

// SiteSearch is the answer.
type SiteSearch struct {
	Results []SearchHit `json:"results"`
	// Searched counts the repositories looked into; Truncated says the
	// limits stopped the search before every visible repository was read.
	Searched  int  `json:"searched"`
	Truncated bool `json:"truncated"`
}

func (s *Server) handleSiteSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < searchMinQuery || len(q) > searchMaxQuery {
		s.problem(w, http.StatusBadRequest, errors.New("q must be 2 to 200 characters"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), searchTimeout)
	defer cancel()
	repos, err := s.st.Repos(ctx)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	repos = visibleRepos(ctx, repos)
	names := make([]string, 0, len(repos))
	for _, n := range repos {
		names = append(names, n.Name)
	}
	slices.Sort(names)
	// The repository on screen first.
	if cur := r.URL.Query().Get("repo"); cur != "" {
		if i := slices.Index(names, cur); i > 0 {
			names = append([]string{cur}, slices.Delete(names, i, i+1)...)
		}
	}
	out := SiteSearch{Results: []SearchHit{}}
	lq := strings.ToLower(q)
	for _, name := range names {
		if strings.Contains(strings.ToLower(name), lq) && len(out.Results) < searchMaxResults {
			out.Results = append(out.Results, SearchHit{Kind: model.KindRepository, Repo: name, Name: name})
		}
	}
	for i, name := range names {
		if i == searchMaxRepos || len(out.Results) >= searchMaxResults || ctx.Err() != nil {
			out.Truncated = true
			break
		}
		hits, err := store.ForRepository(s.st, name).Search(ctx, name, q, siteSearchKinds, searchPerRepo)
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			out.Truncated = true
			break
		}
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		out.Searched++
		for _, n := range hits {
			if len(out.Results) == searchMaxResults {
				out.Truncated = true
				break
			}
			if n.IsTest || isTestPath(n.File) {
				continue
			}
			out.Results = append(out.Results, SearchHit{Kind: n.Kind, Repo: name, ID: n.ID, Name: n.ShortName(),
				Location: n.Location(), File: n.File, Severity: n.Severity})
		}
	}
	writeJSON(w, 200, out)
}

// isTestPath recognises test files by name, for helpers in them that are
// not test functions themselves.
func isTestPath(file string) bool {
	base := path.Base(file)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") ||
		strings.HasSuffix(base, "_test.py") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") ||
		strings.Contains("/"+file, "/__tests__/")
}
