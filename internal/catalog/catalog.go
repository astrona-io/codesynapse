// Package catalog is the one place that answers questions about a
// repository's architecture records. The web API and the MCP tools both call
// it, on a reader already fenced to the repository the caller may see, so
// neither can show more than the other. Every answer says which revision of the
// repository it describes.
package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/arch"
	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Service reads the catalog of one repository at a time.
type Service struct {
	// R must be fenced to the repository (store.ForRepository).
	R store.Reader
	// Items keeps proposals and links; nil leaves them unavailable.
	Items store.CuratedStore
	// Org is the organisation whose proposals and links are shown ("" outside
	// accounts mode). Organisations sharing a code graph never see each other's.
	Org string
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
}

// Source says which revision of the repository an answer describes.
type Source struct {
	Repo       string `json:"repo"`
	Head       string `json:"head,omitempty"`
	IndexedAt  string `json:"indexed_at,omitempty"`
	RecordPath string `json:"path,omitempty"`
}

// Entry is one record in a list.
type Entry struct {
	ID         string   `json:"id"`
	NodeID     string   `json:"node_id"`
	Type       string   `json:"type"`
	Status     string   `json:"status"`
	Binding    bool     `json:"binding"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	Topic      string   `json:"topic,omitempty"`
	Date       string   `json:"date,omitempty"`
	Components []string `json:"components,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Location   string   `json:"location"`
	Supersedes []string `json:"supersedes,omitempty"`
	// SupersededBy lists the records that replace this one.
	SupersededBy []string `json:"superseded_by,omitempty"`
	Fingerprint  string   `json:"fingerprint,omitempty"`
	// Issues counts what the Issues list says about this record.
	Issues int `json:"issues"`
}

// Filter narrows List. Empty fields match everything.
type Filter struct {
	Type, Status, Component, Label, Q string
	// BindingOnly keeps the rules in force: accepted ADRs and active
	// standards and policies that nothing has superseded.
	BindingOnly bool
	Limit       int
	Offset      int
}

// Page is a bounded list with its facets.
type Page struct {
	Entries []Entry `json:"entries"`
	// Total counts the records that matched before Limit and Offset.
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Source Source `json:"source"`
	Facets Facets `json:"facets"`
	Issues int    `json:"issues"`
	// Truncated is set when the repository has more records than can be read.
	Truncated bool `json:"truncated,omitempty"`
}

// Facets count the matched records by the fields a user filters on.
type Facets struct {
	Types      map[string]int `json:"types"`
	Statuses   map[string]int `json:"statuses"`
	Components map[string]int `json:"components"`
	Labels     map[string]int `json:"labels"`
}

// Issue is something about the records that a person should look at. The
// catalog reports; it never decides which of two decisions is right.
type Issue struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"` // warning or info
	Message  string `json:"message"`
	// Records are the IDs involved.
	Records []string `json:"records"`
}

// loaded is every record of a repository with its decoded metadata.
type loaded struct {
	nodes     map[string]model.Node // by record ID
	metas     map[string]arch.Meta
	order     []string
	source    Source
	truncated bool
	// links are the links added through the catalog (this organisation's).
	links []curation.Item
	// extraComponents are components added by applies-to links.
	extraComponents map[string][]string
}

func (s Service) load(ctx context.Context, repo string) (*loaded, error) {
	l := &loaded{nodes: map[string]model.Node{}, metas: map[string]arch.Meta{}, source: Source{Repo: repo}, extraComponents: map[string][]string{}}
	rn, err := s.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, err
	}
	l.source.Head, l.source.IndexedAt = rn.Head, rn.Time
	ranked, err := s.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindRecord}}, store.ByName, store.MaxLimit)
	if err != nil {
		return nil, err
	}
	l.truncated = len(ranked) >= store.MaxLimit
	for _, r := range ranked {
		var m arch.Meta
		if r.Node.Meta != "" {
			if err := json.Unmarshal([]byte(r.Node.Meta), &m); err != nil {
				return nil, fmt.Errorf("decoding record %s: %w", r.Node.QualifiedName, err)
			}
		}
		id := r.Node.QualifiedName
		m.ID = id
		l.nodes[id], l.metas[id] = r.Node, m
		l.order = append(l.order, id)
	}
	slices.Sort(l.order)
	if l.links, err = s.items(ctx, repo, curation.KindLink); err != nil {
		return nil, err
	}
	for _, it := range l.links {
		if k := it.Link; k != nil && k.Kind == curation.AppliesTo {
			id := canonical(l, k.Record)
			l.extraComponents[id] = append(l.extraComponents[id], k.Target)
		}
	}
	return l, nil
}

func (l *loaded) binding(id string) bool {
	n := l.nodes[id]
	return arch.Binding(n.DocType, n.Status) && !l.superseded(id)
}

// superseded is true when an accepted-or-later record replaces this one, even
// if its own status was never updated.
func (l *loaded) superseded(id string) bool {
	for _, by := range l.metas[id].SupersededBy {
		if n, ok := l.nodes[by]; ok && (arch.Binding(n.DocType, n.Status) || n.Status == "implemented" || n.Status == "accepted") {
			return true
		}
	}
	return l.nodes[id].Status == "superseded"
}

func (l *loaded) entry(id string, issues map[string]int) Entry {
	n, m := l.nodes[id], l.metas[id]
	components := m.Components
	if extra := l.extraComponents[id]; len(extra) > 0 {
		components = slices.Compact(slices.Sorted(slices.Values(append(slices.Clone(components), extra...))))
	}
	return Entry{
		ID: id, NodeID: n.ID, Type: n.DocType, Status: n.Status, Binding: l.binding(id), Title: n.Name, Summary: n.Summary,
		Owner: m.Owner, Topic: m.Topic, Date: m.Date, Components: components, Labels: m.Labels, Location: n.Location(),
		Supersedes: m.Supersedes, SupersededBy: m.SupersededBy, Fingerprint: n.BodyHash, Issues: issues[id],
	}
}

// List returns the records that match f, ordered by type, status and ID.
func (s Service) List(ctx context.Context, repo string, f Filter) (Page, error) {
	l, err := s.load(ctx, repo)
	if err != nil {
		return Page{}, err
	}
	issues := l.issues()
	counts := map[string]int{}
	for _, is := range issues {
		for _, id := range is.Records {
			counts[id]++
		}
	}
	p := Page{Entries: []Entry{}, Source: l.source, Truncated: l.truncated, Issues: len(issues),
		Facets: Facets{Types: map[string]int{}, Statuses: map[string]int{}, Components: map[string]int{}, Labels: map[string]int{}}}
	var matched []Entry
	for _, id := range l.order {
		e := l.entry(id, counts)
		if !matches(e, f) {
			continue
		}
		matched = append(matched, e)
		p.Facets.Types[e.Type]++
		p.Facets.Statuses[e.Status]++
		for _, c := range e.Components {
			p.Facets.Components[c]++
		}
		for _, lb := range e.Labels {
			p.Facets.Labels[lb]++
		}
	}
	slices.SortStableFunc(matched, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(typeRank(a.Type), typeRank(b.Type)), cmp.Compare(statusRank(a), statusRank(b)), cmp.Compare(a.ID, b.ID))
	})
	p.Total = len(matched)
	p.Limit = clamp(f.Limit, 50, 200)
	p.Offset = max(0, f.Offset)
	if p.Offset < len(matched) {
		p.Entries = matched[p.Offset:min(len(matched), p.Offset+p.Limit)]
	}
	return p, nil
}

func clamp(v, def, hi int) int {
	if v <= 0 {
		return def
	}
	return min(v, hi)
}

func typeRank(t string) int {
	if i := slices.Index(arch.Types, t); i >= 0 {
		return i
	}
	return len(arch.Types)
}

// statusRank puts the rules in force first, then what is being decided, then history.
func statusRank(e Entry) int {
	switch {
	case e.Binding:
		return 0
	case e.Status == "proposed" || e.Status == "draft" || e.Status == "in-review":
		return 1
	case e.Status == "unknown":
		return 2
	}
	return 3
}

func matches(e Entry, f Filter) bool {
	switch {
	case f.Type != "" && e.Type != f.Type,
		f.Status != "" && e.Status != f.Status,
		f.BindingOnly && !e.Binding,
		f.Component != "" && !slices.Contains(e.Components, strings.ToLower(f.Component)),
		f.Label != "" && !slices.Contains(e.Labels, strings.ToLower(f.Label)):
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		hay := strings.ToLower(strings.Join([]string{e.ID, e.Title, e.Summary, e.Topic, strings.Join(e.Components, " "), strings.Join(e.Labels, " "), e.Owner}, " "))
		for _, w := range strings.Fields(q) {
			if !strings.Contains(hay, w) {
				return false
			}
		}
	}
	return true
}

// Issues lists what a person should look at in the catalog.
func (s Service) Issues(ctx context.Context, repo string) ([]Issue, Source, error) {
	l, err := s.load(ctx, repo)
	if err != nil {
		return nil, Source{}, err
	}
	out := l.issues()
	for i, it := range l.links {
		if i >= maxLinks {
			break
		}
		if k := it.Link; k != nil && k.Kind == curation.Implements {
			if _, err := s.resolveCode(ctx, repo, k.Target); err != nil {
				out = append(out, Issue{Kind: "stale-link", Severity: "warning", Records: []string{canonical(l, k.Record)},
					Message: fmt.Sprintf("the link from %s to %q no longer resolves: %v. Re-link it to the code's new name, or remove it", canonical(l, k.Record), k.Target, err)})
			}
		}
	}
	return out, l.source, nil
}

func (l *loaded) issues() []Issue {
	out := []Issue{}
	add := func(kind, msg string, ids ...string) {
		out = append(out, Issue{Kind: kind, Severity: "warning", Message: msg, Records: ids})
	}
	for _, id := range l.order {
		n, m := l.nodes[id], l.metas[id]
		for _, u := range m.Unresolved {
			kind := "unresolved-reference"
			if strings.HasPrefix(u, "status") || strings.HasPrefix(u, "no status") {
				kind = "status-unknown"
			}
			add(kind, fmt.Sprintf("%s: %s", id, u), id)
		}
		for _, by := range m.SupersededBy {
			succ, ok := l.nodes[by]
			if !ok {
				continue
			}
			if n.Status != "superseded" && n.Status != "deprecated" && n.Status != "retired" && n.Status != "rejected" {
				add("supersession-mismatch", fmt.Sprintf("%s is replaced by %s but its status is %s, not superseded", id, by, n.Status), id, by)
			}
			if slices.Contains([]string{"proposed", "draft", "in-review", arch.Unknown}, succ.Status) {
				add("successor-not-in-force", fmt.Sprintf("%s supersedes %s but is itself %s, so nothing is in force for that decision", by, id, succ.Status), by, id)
			}
		}
		if n.Status == "superseded" && len(m.SupersededBy) == 0 {
			add("superseded-without-successor", fmt.Sprintf("%s is superseded but no record says which one replaces it", id), id)
		}
		for _, c := range m.Conflicts {
			if _, ok := l.nodes[c]; ok && (l.binding(id) || l.binding(c)) && id < c {
				add("explicit-conflict", fmt.Sprintf("%s says it conflicts with %s", id, c), id, c)
			}
		}
	}
	for _, cycle := range l.cycles() {
		add("supersession-cycle", "records supersede each other in a cycle: "+strings.Join(cycle, " to "), cycle...)
	}
	// Two rules in force on the same topic for the same component may disagree.
	type key struct{ component, topic string }
	groups := map[key][]string{}
	for _, id := range l.order {
		m := l.metas[id]
		if !l.binding(id) || m.Topic == "" {
			continue
		}
		comps := l.entry(id, nil).Components
		if len(comps) == 0 {
			comps = []string{"(whole system)"}
		}
		for _, c := range comps {
			groups[key{c, m.Topic}] = append(groups[key{c, m.Topic}], id)
		}
	}
	keys := make([]key, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.component, b.component), cmp.Compare(a.topic, b.topic))
	})
	for _, k := range keys {
		if ids := groups[k]; len(ids) > 1 {
			add("potential-conflict", fmt.Sprintf("%s are all in force for the topic %q of %s: check that they agree, or supersede the ones that no longer apply", strings.Join(ids, ", "), k.topic, k.component), ids...)
		}
	}
	slices.SortStableFunc(out, func(a, b Issue) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(strings.Join(a.Records, ","), strings.Join(b.Records, ",")))
	})
	return out
}

// cycles finds supersession cycles; each is reported once, as the IDs in order.
func (l *loaded) cycles() [][]string {
	var found [][]string
	state := map[string]int{} // 1 visiting, 2 done
	var stack []string
	var walk func(id string)
	walk = func(id string) {
		state[id] = 1
		stack = append(stack, id)
		for _, next := range l.metas[id].Supersedes {
			if _, ok := l.nodes[next]; !ok {
				continue
			}
			switch state[next] {
			case 0:
				walk(next)
			case 1:
				i := slices.Index(stack, next)
				cyc := slices.Clone(stack[i:])
				// Rotate to the smallest ID so the same cycle is not reported twice.
				least := 0
				for j := range cyc {
					if cyc[j] < cyc[least] {
						least = j
					}
				}
				cyc = append(cyc[least:], cyc[:least]...)
				if !slices.ContainsFunc(found, func(f []string) bool { return slices.Equal(f, cyc) }) {
					found = append(found, cyc)
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
	}
	for _, id := range l.order {
		if state[id] == 0 {
			walk(id)
		}
	}
	return found
}

// ChainLink is one step in the supersession chain of a record.
type ChainLink struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Relation string `json:"relation"` // supersedes or superseded-by, as seen from the record
	Depth    int    `json:"depth"`
}

// CodeLink is code that carries out a record, or code its text mentions.
type CodeLink struct {
	query.Ref
	// Declared is how the record or the link named it.
	Declared string `json:"declared,omitempty"`
	// Source is "record" (declared in the file) or "link" (added through the
	// catalog by a person or agent: By says who, LinkID removes it).
	Source string `json:"source"`
	LinkID string `json:"link_id,omitempty"`
	By     string `json:"by,omitempty"`
	// Stale is set when a link no longer resolves to exactly one place.
	Stale   bool   `json:"stale,omitempty"`
	Problem string `json:"problem,omitempty"`
}

// Revision is a commit that changed the record's file.
type Revision struct {
	Commit  string `json:"commit"`
	Time    string `json:"time,omitempty"`
	Subject string `json:"subject,omitempty"`
}

// Section is a heading of the record's file.
type Section struct {
	Name  string `json:"name"`
	Lines string `json:"lines"`
	Level int    `json:"level,omitempty"`
}

// Detail is everything known about one record.
type Detail struct {
	Entry
	RawStatus string      `json:"raw_status,omitempty"`
	Format    string      `json:"format,omitempty"`
	Lifecycle []string    `json:"lifecycle"`
	Chain     []ChainLink `json:"chain"`
	// ImplementedBy is code the record names or that a person linked to it.
	ImplementedBy []CodeLink `json:"implemented_by"`
	// Mentions is code whose name appears in the record's text; it is
	// weaker than ImplementedBy and never presented as an implementation.
	Mentions []query.Ref `json:"mentions"`
	// Links are the links added through the catalog that touch this record.
	Links     []LinkView `json:"links"`
	Related   []Entry    `json:"related"`
	Problems  []Issue    `json:"problems"`
	Outline   []Section  `json:"outline"`
	Revisions []Revision `json:"revisions"`
	Source    Source     `json:"source"`
	// Unresolved are the references in the record that matched nothing.
	Unresolved []string `json:"unresolved,omitempty"`
}

// ErrNotFound is returned for an unknown record ID.
var ErrNotFound = fmt.Errorf("no such record: %w", store.ErrNotFound)

const maxLinks = 200

// Get returns one record by ID.
func (s Service) Get(ctx context.Context, repo, id string) (Detail, error) {
	l, err := s.load(ctx, repo)
	if err != nil {
		return Detail{}, err
	}
	var key string
	for _, k := range l.order {
		if strings.EqualFold(k, id) {
			key = k
		}
	}
	if key == "" {
		return Detail{}, ErrNotFound
	}
	issues := l.issues()
	counts := map[string]int{}
	var mine []Issue
	for _, is := range issues {
		for _, r := range is.Records {
			counts[r]++
		}
		if slices.Contains(is.Records, key) {
			mine = append(mine, is)
		}
	}
	n, m := l.nodes[key], l.metas[key]
	d := Detail{Entry: l.entry(key, counts), RawStatus: m.RawStatus, Format: m.Format, Lifecycle: arch.StatusesOf(n.DocType),
		Chain: l.chain(key), ImplementedBy: []CodeLink{}, Mentions: []query.Ref{}, Related: []Entry{}, Problems: mine, Outline: []Section{}, Revisions: []Revision{},
		Source: Source{Repo: repo, Head: l.source.Head, IndexedAt: l.source.IndexedAt, RecordPath: n.File}, Unresolved: m.Unresolved}
	if d.Problems == nil {
		d.Problems = []Issue{}
	}
	for _, rel := range m.Relates {
		if _, ok := l.nodes[rel]; ok {
			d.Related = append(d.Related, l.entry(rel, counts))
		}
	}

	declared := map[string]string{}
	for _, c := range m.Implements {
		for _, nodeID := range c.Resolved {
			declared[nodeID] = c.Ref
		}
	}
	hits, err := s.R.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeImplementsDecision}, store.In, 1, maxLinks)
	if err != nil {
		return d, err
	}
	for _, h := range hits {
		d.ImplementedBy = append(d.ImplementedBy, CodeLink{Ref: query.RefOf(h.Node), Declared: declared[h.Node.ID], Source: "record"})
	}
	d.Links = []LinkView{}
	for _, it := range l.links {
		k := it.Link
		if k == nil {
			continue
		}
		touches := strings.EqualFold(k.Record, key) || (k.Kind == curation.Relates && strings.EqualFold(k.Target, key))
		if !touches {
			continue
		}
		v := s.linkView(ctx, repo, it)
		d.Links = append(d.Links, v)
		switch k.Kind {
		case curation.Implements:
			by := cmp.Or(v.Client, v.CreatedBy)
			if v.Stale {
				d.ImplementedBy = append(d.ImplementedBy, CodeLink{Ref: query.Ref{Name: k.Target}, Declared: k.Target, Source: "link", LinkID: v.ID, By: by, Stale: true, Problem: v.Problem})
			} else if !slices.ContainsFunc(d.ImplementedBy, func(c CodeLink) bool { return c.ID == v.Resolved[0].ID }) {
				d.ImplementedBy = append(d.ImplementedBy, CodeLink{Ref: v.Resolved[0], Declared: k.Target, Source: "link", LinkID: v.ID, By: by})
			}
		case curation.Relates:
			other := canonical(l, k.Target)
			if strings.EqualFold(k.Target, key) {
				other = canonical(l, k.Record)
			}
			if _, ok := l.nodes[other]; ok && !slices.ContainsFunc(d.Related, func(e Entry) bool { return e.ID == other }) {
				d.Related = append(d.Related, l.entry(other, counts))
			}
		}
	}
	slices.SortFunc(d.ImplementedBy, func(a, b CodeLink) int {
		return cmp.Or(cmp.Compare(boolRank(a.Stale), boolRank(b.Stale)), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	})

	sections, err := s.R.NodesInFile(ctx, repo, n.File)
	if err != nil {
		return d, err
	}
	seen := map[string]bool{}
	for _, sec := range sections {
		if sec.Kind != model.KindSection {
			continue
		}
		d.Outline = append(d.Outline, Section{Name: sec.Name, Lines: fmt.Sprintf("%d-%d", sec.StartLine, max(sec.StartLine, sec.EndLine)), Level: sec.Level})
		ms, err := s.R.Traverse(ctx, sec.ID, []model.EdgeKind{model.EdgeMentions}, store.Out, 1, maxLinks)
		if err != nil {
			return d, err
		}
		for _, h := range ms {
			if !seen[h.Node.ID] && len(d.Mentions) < maxLinks {
				seen[h.Node.ID] = true
				d.Mentions = append(d.Mentions, query.RefOf(h.Node))
			}
		}
	}
	if commits, err := s.R.Traverse(ctx, model.NodeID(repo, model.KindFile, n.File), []model.EdgeKind{model.EdgeModifies}, store.In, 1, 10); err == nil {
		for _, h := range commits {
			d.Revisions = append(d.Revisions, Revision{Commit: h.Node.QualifiedName, Time: h.Node.Time, Subject: h.Node.Summary})
		}
		slices.SortFunc(d.Revisions, func(a, b Revision) int { return cmp.Compare(b.Time, a.Time) })
	}
	return d, nil
}

// chain walks the supersession links in both directions from a record.
func (l *loaded) chain(id string) []ChainLink {
	out := []ChainLink{}
	seen := map[string]bool{id: true}
	var walk func(from, relation string, depth int, next func(string) []string)
	walk = func(from, relation string, depth int, next func(string) []string) {
		for _, to := range next(from) {
			if seen[to] {
				continue
			}
			n, ok := l.nodes[to]
			if !ok {
				continue
			}
			seen[to] = true
			out = append(out, ChainLink{ID: to, Title: n.Name, Status: n.Status, Relation: relation, Depth: depth})
			walk(to, relation, depth+1, next)
		}
	}
	walk(id, "superseded-by", 1, func(x string) []string { return l.metas[x].SupersededBy })
	walk(id, "supersedes", 1, func(x string) []string { return l.metas[x].Supersedes })
	return out
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
