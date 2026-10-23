package agentctx

import (
	"context"
	"errors"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// AnalysisNote says how far the graph around a symbol can be trusted: the
// level and status of its file's analysis and the calls in its body the
// extractor could not follow. Nil when the graph predates the records.
type AnalysisNote struct {
	// Level is typed (go/types) or syntactic (structural parser).
	Level string `json:"level,omitempty"`
	// Status is complete, partial (source errors) or truncated (work budget).
	Status string `json:"status,omitempty"`
	// UnresolvedCalls are call sites through function values: callees may be
	// missing.
	UnresolvedCalls int `json:"unresolved_calls,omitempty"`
	// DataflowCalls of those were linked by the optional data-flow analysis
	// to callees marked with dataflow evidence.
	DataflowCalls int `json:"dataflow_calls,omitempty"`
	// OmittedBody is set when the body was not analysed at all.
	OmittedBody bool `json:"omitted_body,omitempty"`
}

// analysisOf combines the symbol's own record with its file's.
func (s Service) analysisOf(ctx context.Context, repo string, n model.Node) (*AnalysisNote, error) {
	var note AnalysisNote
	own, err := model.DecodeAnalysis(n.Analysis)
	if err != nil {
		return nil, nil //nolint:nilerr // a damaged record is the same as none
	}
	note.UnresolvedCalls, note.DataflowCalls, note.OmittedBody = own.UnresolvedCalls, own.DataflowCalls, own.OmittedBodies > 0
	if n.File != "" {
		f, err := s.R.Node(ctx, model.NodeID(repo, model.KindFile, n.File))
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return nil, err
		default:
			if fa, err := model.DecodeAnalysis(f.Analysis); err == nil {
				note.Level, note.Status = fa.Level, fa.Status
			}
		}
	}
	if note == (AnalysisNote{}) {
		return nil, nil
	}
	return &note, nil
}

// labelEvidence sets Evidence on the direct (depth 1) entries of callers and
// callees, which are refs built from the same hits in the same order.
func (s Service) labelEvidence(ctx context.Context, root string, callers []model.Hit, callerRefs []Ref, callees []model.Hit, calleeRefs []Ref) error {
	ids := []string{root}
	for i := range callerRefs {
		if callers[i].Depth == 1 {
			ids = append(ids, callers[i].Node.ID)
		}
	}
	for i := range calleeRefs {
		if callees[i].Depth == 1 {
			ids = append(ids, callees[i].Node.ID)
		}
	}
	if len(ids) == 1 {
		return nil
	}
	edges, err := s.R.Subgraph(ctx, ids, callEdges)
	if err != nil {
		return err
	}
	in, out := map[string]model.Evidence{}, map[string]model.Evidence{}
	keep := func(m map[string]model.Evidence, id string, e model.Evidence) {
		if cur, ok := m[id]; !ok || e.Stronger(cur) {
			m[id] = e
		}
	}
	for _, e := range edges {
		switch {
		case e.To == root:
			keep(in, e.From, e.Evidence)
		case e.From == root && e.Kind == model.EdgeCalls:
			keep(out, e.To, e.Evidence)
		}
	}
	label := func(hits []model.Hit, refs []Ref, m map[string]model.Evidence) {
		for i := range refs {
			if e, ok := m[hits[i].Node.ID]; ok && hits[i].Depth == 1 {
				refs[i].Evidence = e.OrUnknown()
			}
		}
	}
	label(callers, callerRefs, in)
	label(callees, calleeRefs, out)
	return nil
}
