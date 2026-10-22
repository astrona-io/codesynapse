package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// maxGraphIDs bounds the node set a browser graph can ask edges for.
const maxGraphIDs = 1500

// structuralEdges are expanded by default in the graph explorer. AUTHORED is
// left out because authors connect to every commit and drown the view.
var structuralEdges = []model.EdgeKind{
	model.EdgeCalls, model.EdgeDispatchesTo, model.EdgeImplements, model.EdgeHasMethod,
	model.EdgeDefines, model.EdgeImports, model.EdgeMentions, model.EdgeContains, model.EdgeModifies,
	model.EdgeHasFinding, model.EdgeAffects, model.EdgeDependsOn, model.EdgeIncludes, model.EdgeUsesValue,
}

func parseEdgeKinds(raw []string) ([]model.EdgeKind, error) {
	var out []model.EdgeKind
	for _, r := range raw {
		for _, part := range strings.Split(r, ",") {
			k := model.EdgeKind(strings.TrimSpace(part))
			if k == "" {
				continue
			}
			if !k.Valid() {
				return nil, errors.New("unknown edge kind " + string(k))
			}
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return structuralEdges, nil
	}
	return out, nil
}

// handleGraphOverview seeds the explorer with the most-called symbols of a
// repository and the edges between them.
func (s *Server) handleGraphOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repo := r.PathValue("repo")
	kinds, err := parseKinds(r.URL.Query()["kind"])
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	if len(kinds) == 0 {
		kinds = []model.NodeKind{model.KindFunction, model.KindMethod, model.KindInterfaceMethod, model.KindType}
	}
	edges, err := parseEdgeKinds(r.URL.Query()["edge"])
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	limit := min(intParam(r, "limit", 120), 400)
	ranked, err := s.st.List(ctx, repo, store.Filter{Kinds: kinds, ExcludeTests: r.URL.Query().Get("tests") != "1"}, store.ByFanIn, limit)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	view := GraphView{Nodes: make([]GraphNode, 0, len(ranked))}
	ids := make([]string, 0, len(ranked))
	for _, rk := range ranked {
		ids = append(ids, rk.Node.ID)
		view.Nodes = append(view.Nodes, graphNode(rk.Node, 0))
	}
	view.Edges, err = store.ForRepository(s.st, r.PathValue("repo")).Subgraph(ctx, ids, edges)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, view)
}

// handleGraphExpand returns a node and its direct neighbours over the given
// edge kinds, in both directions. The client merges them into its view and
// then asks /graph/edges for edges to nodes it already shows.
func (s *Server) handleGraphExpand(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	root, err := s.repoQuery(r.PathValue("repo")).Resolve(ctx, r.PathValue("repo"), r.URL.Query().Get("id"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	edges, err := parseEdgeKinds(r.URL.Query()["edge"])
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	limit := min(intParam(r, "limit", 40), 150)
	view := GraphView{Root: root.ID, Nodes: []GraphNode{graphNode(root, 0)}}
	seen := map[string]bool{root.ID: true}
	for _, dir := range []store.Direction{store.In, store.Out} {
		hits, err := store.ForRepository(s.st, r.PathValue("repo")).Traverse(ctx, root.ID, edges, dir, 1, limit)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		for _, h := range hits {
			if !seen[h.Node.ID] {
				seen[h.Node.ID] = true
				view.Nodes = append(view.Nodes, graphNode(h.Node, 1))
			}
		}
	}
	view.Edges = []model.Edge{}
	writeJSON(w, 200, view)
}

type edgesRequest struct {
	IDs   []string `json:"ids"`
	Edges []string `json:"edges"`
}

// handleGraphEdges returns the edges among a set of node IDs.
func (s *Server) handleGraphEdges(w http.ResponseWriter, r *http.Request) {
	var req edgesRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if len(req.IDs) > maxGraphIDs {
		s.problem(w, 400, errors.New("too many ids"))
		return
	}
	repoPrefix := r.PathValue("repo") + "|"
	for _, id := range req.IDs {
		if !strings.HasPrefix(id, repoPrefix) {
			s.problem(w, 400, errors.New("ids must belong to the repository in the path"))
			return
		}
	}
	edges, err := parseEdgeKinds(req.Edges)
	if err != nil {
		s.problem(w, 400, err)
		return
	}
	out, err := store.ForRepository(s.st, r.PathValue("repo")).Subgraph(r.Context(), req.IDs, edges)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if out == nil {
		out = []model.Edge{}
	}
	writeJSON(w, 200, map[string]any{"edges": out})
}
