package ingest

import "github.com/astrona-io/codesynapse/internal/model"

// analysisSummary totals what the extractors recorded: how many files were
// analysed partially or truncated, what packages left out, and how many
// resolution edges rest on each kind of evidence. Go files carry their
// package's status, so statuses are counted per file and omissions per
// package.
func analysisSummary(g *model.Graph) model.Analysis {
	sum := model.Analysis{Edges: map[model.Evidence]int{}}
	for _, n := range g.Nodes() {
		if n.Kind != model.KindPackage && n.Kind != model.KindFile || n.Analysis == "" {
			continue
		}
		a, err := model.DecodeAnalysis(n.Analysis)
		if err != nil {
			continue // written by this process; cannot happen
		}
		if n.Kind == model.KindFile {
			switch a.Status {
			case model.StatusPartial:
				sum.Partial++
			case model.StatusTruncated:
				sum.Truncated++
			}
			continue
		}
		sum.Errors += a.Errors
		sum.IgnoredFiles += a.IgnoredFiles
		sum.UnresolvedCalls += a.UnresolvedCalls
		sum.OmittedBodies += a.OmittedBodies
		sum.DataflowCalls += a.DataflowCalls
	}
	for _, e := range g.Edges() {
		if model.ResolutionEdge(e.Kind) {
			sum.Edges[e.Evidence.OrUnknown()]++
		}
	}
	return sum
}
