package api

import (
	"errors"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/catalog"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Sections counts what a repository has, so the dashboard's repository menu
// shows the pages with something in them and keeps the rest under "More"
// (docs/PLAN.md, roadmap item 10). Counts only, from the stored graph and
// reports: no content, and no work beyond a few bounded reads.
type Sections struct {
	Packages     int `json:"packages"`
	Symbols      int `json:"symbols"`
	Documents    int `json:"documents"`
	ConfigFiles  int `json:"config_files"`
	K8sResources int `json:"k8s_resources"`
	HelmCharts   int `json:"helm_charts"`
	Records      int `json:"records"`
	// Proposals counts the catalog's proposals the caller's organisation
	// sees; 0 while the catalog is turned off.
	Proposals    int  `json:"proposals"`
	Dependencies int  `json:"dependencies"`
	Licences     bool `json:"licences"`
	Findings     int  `json:"findings"`
	Snapshots    int  `json:"snapshots"`
	Reviews      int  `json:"reviews"`
	MemoryFacts  int  `json:"memory_facts"`
	// Admin says the caller administers the repository (always true in
	// single-tenant mode).
	Admin bool `json:"admin"`
}

// sectionsCap bounds the list reads; the menu only needs "some".
const sectionsCap = 100

func (s *Server) handleSections(w http.ResponseWriter, r *http.Request) {
	ctx, repo := r.Context(), r.PathValue("repo")
	repoNode, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	st, err := store.ForRepository(s.st, repo).Stats(ctx, repo)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	n := st.Nodes
	out := Sections{
		Packages:     n[model.KindPackage],
		Symbols:      n[model.KindFunction] + n[model.KindMethod] + n[model.KindInterfaceMethod] + n[model.KindType],
		Documents:    n[model.KindDocument],
		ConfigFiles:  n[model.KindConfig] + n[model.KindTemplate],
		K8sResources: st.K8sResources,
		HelmCharts:   st.HelmCharts,
		Records:      n[model.KindRecord] + n[model.KindComponent],
		Dependencies: n[model.KindModule],
		Licences:     repoNode.Licenses != "",
		Findings:     n[model.KindFinding],
	}
	props, err := s.catalogOf(ctx, repo).Proposals(ctx, repo)
	if err != nil && !errors.Is(err, catalog.ErrDisabled) {
		s.problem(w, 500, err)
		return
	}
	out.Proposals = len(props)
	snaps, err := s.st.Snapshots(ctx, repo, sectionsCap)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out.Snapshots = len(snaps)
	list, err := s.st.ListReviews(ctx, repo, sectionsCap)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	sc := scopeFrom(ctx)
	for _, rv := range list {
		if seesReview(sc, rv.Repo, rv.OrgID) {
			out.Reviews++
		}
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	mem, err := s.st.SearchMemory(ctx, ns, "", sectionsCap, false)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out.MemoryFacts = len(mem.Facts)
	out.Admin = sc.all || sc.graphs[repo].AtLeast(accounts.Admin)
	writeJSON(w, 200, out)
}
