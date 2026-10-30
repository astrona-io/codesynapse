package api

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Indexed files by language or format across the platform
// (docs/plans/PLATFORM_STATS_PLAN.md, section 4): the format counts of the
// latest snapshot of every graph an organisation holds (snapshot.Take reads
// the graph's Stats once per ingest and keeps only known names), each graph
// counted once whatever the number of organisations holding it. Entries
// whose access was lost are left out. A format is named only when at least
// minGroupOrgs organisations hold graphs with it, then the top
// maxNamedValues; the rest is "other".

// PlatformFormatCount is the number of indexed files of one language or
// format (or "other") across the platform's graphs.
type PlatformFormatCount struct {
	Name  string `json:"name"`
	Files int    `json:"files"`
}

// platformFormats counts the files by format of every usable graph, and the
// graphs whose latest snapshot has no format counts (none taken yet, or
// taken before snapshots kept them).
func (s *Server) platformFormats(ctx context.Context) ([]PlatformFormatCount, int, error) {
	orgs, entries, graphs, err := s.platformGraphs(ctx)
	if err != nil {
		return nil, 0, err
	}
	latest := make([]map[string]int, len(graphs))
	err = forEach(ctx, len(graphs), func(ctx context.Context, i int) error {
		snaps, err := s.st.Snapshots(ctx, graphs[i], 1)
		if err != nil {
			return err
		}
		if len(snaps) > 0 {
			latest[i] = snaps[len(snaps)-1].Formats
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	index := make(map[string]int, len(graphs))
	for i, g := range graphs {
		index[g] = i
	}
	b := newBreakdown()
	counted := map[string]bool{}
	unknown := 0
	for _, o := range orgs {
		for _, g := range entries[o.ID] {
			formats := latest[index[g]]
			for name, n := range formats {
				markRule(b, o.ID, name, n, !counted[g])
			}
			if !counted[g] && len(formats) == 0 {
				unknown++
			}
			counted[g] = true
		}
	}
	out := []PlatformFormatCount{}
	for _, c := range b.fold(true) {
		out = append(out, PlatformFormatCount{Name: c.Name, Files: c.Calls})
	}
	return out, unknown, nil
}

// ingestFormats adds the platform's formats to the ingest section; one
// organisation selected has none, like the rule breakdown of security.
func (s *Server) ingestFormats(ctx context.Context, q accounts.StatsQuery, d *ingestData) error {
	if q.OrgID != "" {
		return nil
	}
	var err error
	d.formats, d.formatsUnknown, err = s.platformFormats(ctx)
	return err
}
