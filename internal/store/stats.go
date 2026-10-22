package store

import "github.com/parisnakitakejser/codesynapse/internal/model"

// StatsBuilder applies the Stats rules every store shares. Stores that
// aggregate in the database feed it groups: a node standing for count alike
// nodes, with LOC and Complexity holding the group's sums.
type StatsBuilder struct {
	st                       model.Stats
	complexitySum, callables int
	charts                   map[string]bool
}

// NewStatsBuilder starts the statistics of repo.
func NewStatsBuilder(repo string) *StatsBuilder {
	return &StatsBuilder{st: model.Stats{Repo: repo, Nodes: map[model.NodeKind]int{}, Edges: map[model.EdgeKind]int{},
		SecurityTags: map[string]int{}, Formats: map[string]int{}}, charts: map[string]bool{}}
}

// Add counts count nodes like n. n.SecurityTags count once per node.
func (b *StatsBuilder) Add(n model.Node, count int) {
	st := &b.st
	st.Nodes[n.Kind] += count
	if f := model.FileFormat(n.Kind, n.Language, n.Package != ""); f != "" {
		st.Formats[f] += count
	}
	if n.Kind == model.KindTemplate && n.Package != "" {
		b.charts[n.Package] = true
	}
	if model.IsK8sResource(n) {
		st.K8sResources += count
	}
	switch n.Kind {
	case model.KindFile:
		st.TotalLOC += n.LOC
	case model.KindFunction, model.KindMethod:
		if n.IsTest {
			st.Tests += count
			return
		}
		b.complexitySum += n.Complexity
		b.callables += count
		for _, t := range n.SecurityTags {
			st.SecurityTags[t] += count
		}
	}
	if (n.Kind == model.KindFunction || n.Kind == model.KindMethod || n.Kind == model.KindType) && n.Exported && !n.IsTest {
		st.Exported += count
		if n.Summary != "" {
			st.Documented += count
		}
	}
}

// AddTags adds security tag counts aggregated elsewhere (non-test functions
// and methods only).
func (b *StatsBuilder) AddTags(tags map[string]int) {
	for t, c := range tags {
		b.st.SecurityTags[t] += c
	}
}

// AddEdges counts c edges of kind.
func (b *StatsBuilder) AddEdges(kind model.EdgeKind, c int) { b.st.Edges[kind] += c }

// Stats returns the result.
func (b *StatsBuilder) Stats() model.Stats {
	st := b.st
	st.HelmCharts = len(b.charts)
	if b.callables > 0 {
		st.AvgComplexity = float64(b.complexitySum) / float64(b.callables)
	}
	return st
}
