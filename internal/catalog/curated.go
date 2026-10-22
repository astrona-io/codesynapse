package catalog

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/arch"
	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Actor is who adds something: the organisation that owns the work, the user
// and the client (an MCP client name, or "web").
type Actor struct {
	Org, User, Client string
}

// ErrDisabled is returned when the service has nowhere to keep proposals and links.
var ErrDisabled = errors.New("proposals and links are not available on this server")

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// items returns the proposals or links of the repository that belong to the
// service's organisation. Organisations that share a code graph never see
// each other's.
func (s Service) items(ctx context.Context, repo, kind string) ([]curation.Item, error) {
	if s.Items == nil {
		return nil, nil
	}
	all, err := s.Items.ListCurated(ctx, repo, kind, curation.MaxItemsPerRepo)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(it curation.Item) bool { return it.OrgID != s.Org }), nil
}

// CodeCheck says whether a code reference matched exactly one place.
type CodeCheck struct {
	Ref   string `json:"ref"`
	Found bool   `json:"found"`
	// Names are the places it matched (one when Found).
	Names []string `json:"names,omitempty"`
	Why   string   `json:"why,omitempty"`
}

// ProposalView is a proposal as shown: always a draft, never a rule.
type ProposalView struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Status is the draft word of the type; State is open, withdrawn or
	// merged (a record with the suggested ID and title now exists).
	Status        string      `json:"status"`
	State         string      `json:"state"`
	Binding       bool        `json:"binding"`
	Topic         string      `json:"topic,omitempty"`
	Components    []string    `json:"components,omitempty"`
	Labels        []string    `json:"labels,omitempty"`
	Supersedes    []string    `json:"supersedes,omitempty"`
	Relates       []string    `json:"relates,omitempty"`
	Implements    []CodeCheck `json:"implements,omitempty"`
	SuggestedID   string      `json:"suggested_id"`
	SuggestedPath string      `json:"suggested_path"`
	CreatedAt     string      `json:"created_at"`
	CreatedBy     string      `json:"created_by,omitempty"`
	Client        string      `json:"client,omitempty"`
	Notes         []string    `json:"notes"`
}

// LinkView is a link a person or agent added.
type LinkView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Record    string `json:"record"`
	Target    string `json:"target"`
	Note      string `json:"note,omitempty"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by,omitempty"`
	Client    string `json:"client,omitempty"`
	// Resolved is the code an implements link points at now. Stale is set when
	// it no longer resolves to exactly one place (renamed, removed, ambiguous):
	// the link is kept and flagged, never silently dropped.
	Resolved []query.Ref `json:"resolved,omitempty"`
	Stale    bool        `json:"stale,omitempty"`
	Problem  string      `json:"problem,omitempty"`
}

// resolveCode finds the one place a code reference names: a file, a symbol or
// a package (by import path or path suffix).
func (s Service) resolveCode(ctx context.Context, repo, ref string) (model.Node, error) {
	ref = strings.TrimSpace(strings.Trim(ref, "`"))
	clean := strings.TrimPrefix(path.Clean(ref), "./")
	if n, err := s.R.Node(ctx, model.NodeID(repo, model.KindFile, clean)); err == nil {
		return n, nil
	}
	if n, err := (query.Service{R: s.R}).Resolve(ctx, repo, ref); err == nil {
		return n, nil
	} else if strings.Contains(err.Error(), "ambiguous") || strings.Contains(err.Error(), "matches") {
		return model.Node{}, err
	}
	pkgs, err := s.R.Search(ctx, repo, path.Base(clean), []model.NodeKind{model.KindPackage}, 20)
	if err != nil {
		return model.Node{}, err
	}
	var hits []model.Node
	for _, p := range pkgs {
		if p.QualifiedName == ref || strings.HasSuffix(p.QualifiedName, "/"+clean) || p.QualifiedName == clean {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return model.Node{}, fmt.Errorf("nothing in the code is called %q (file, symbol or package)", ref)
	}
	return model.Node{}, fmt.Errorf("%q matches %d packages; use a longer path", ref, len(hits))
}

func (s Service) check(ctx context.Context, repo, ref string) CodeCheck {
	n, err := s.resolveCode(ctx, repo, ref)
	if err != nil {
		return CodeCheck{Ref: ref, Why: err.Error()}
	}
	return CodeCheck{Ref: ref, Found: true, Names: []string{n.ShortName()}}
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugOf(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if s == "" {
		return "untitled"
	}
	return s[:min(len(s), 48)]
}

var dirOfType = map[string]string{arch.ADR: "docs/adr", arch.RFC: "docs/rfcs", arch.Standard: "docs/standards", arch.Policy: "docs/policies"}

// suggest says which ID and file a record of this type and title would get
// next, from the records already in the repository.
func (l *loaded) suggest(docType, title string) (id, filePath string) {
	dirs := map[string]int{}
	next := 1
	re := regexp.MustCompile(`^(?i)` + docType + `-0*(\d+)$`)
	for _, rid := range l.order {
		n := l.nodes[rid]
		if n.DocType != docType {
			continue
		}
		dirs[path.Dir(n.File)]++
		if m := re.FindStringSubmatch(rid); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil && v >= next {
				next = v + 1
			}
		}
	}
	dir, best := dirOfType[docType], 0
	for d, c := range dirs {
		if c > best || c == best && d < dir {
			dir, best = d, c
		}
	}
	slug := slugOf(title)
	if docType == arch.ADR || docType == arch.RFC {
		file := fmt.Sprintf("%04d-%s.md", next, slug)
		return arch.DefaultID(docType, path.Join(dir, file), fmt.Sprintf("%04d", next)), path.Join(dir, file)
	}
	filePath = path.Join(dir, slug+".md")
	return arch.DefaultID(docType, filePath, ""), filePath
}

func (s Service) proposalView(ctx context.Context, repo string, it curation.Item, l *loaded, withBody bool) ProposalView {
	p := it.Proposal
	id, filePath := l.suggest(p.Type, p.Title)
	v := ProposalView{ID: it.ID, Type: p.Type, Title: p.Title, Status: p.Status, State: p.State, Topic: p.Topic, Components: p.Components,
		Labels: p.Labels, Supersedes: p.Supersedes, Relates: p.Relates, SuggestedID: id, SuggestedPath: filePath,
		CreatedAt: it.CreatedAt, CreatedBy: it.CreatedBy, Client: it.Client,
		Notes: []string{"A proposal is not a decision. Nothing is in force until someone commits it as a file with an accepted status."}}
	if withBody {
		v.Body = p.Body
	}
	for _, ref := range p.Implements {
		v.Implements = append(v.Implements, s.check(ctx, repo, ref))
	}
	if p.State == curation.Open {
		for rid, n := range l.nodes {
			if n.DocType == p.Type && strings.EqualFold(n.Name, p.Title) {
				v.State, v.SuggestedID, v.SuggestedPath = "merged", rid, n.File
				v.Notes = append(v.Notes, fmt.Sprintf("A record with this title is now in the repository as %s (%s).", rid, n.Status))
				break
			}
		}
	}
	return v
}

// Propose stores a draft record. It validates what it can check: the types,
// that superseded and related records exist, and that code references name
// exactly one place. It cannot approve anything.
func (s Service) Propose(ctx context.Context, repo string, who Actor, in curation.ProposalInput) (ProposalView, error) {
	if s.Items == nil {
		return ProposalView{}, ErrDisabled
	}
	l, err := s.load(ctx, repo)
	if err != nil {
		return ProposalView{}, err
	}
	known := func(ids []string, what string) error {
		for _, id := range ids {
			if _, ok := l.nodes[canonical(l, id)]; !ok {
				return fmt.Errorf("%w: %s %q is not a record in this repository", curation.ErrInvalid, what, id)
			}
		}
		return nil
	}
	if err := known(in.Supersedes, "supersedes"); err != nil {
		return ProposalView{}, err
	}
	if err := known(in.Relates, "relates"); err != nil {
		return ProposalView{}, err
	}
	for _, ref := range in.Implements {
		if c := s.check(ctx, repo, ref); !c.Found {
			return ProposalView{}, fmt.Errorf("%w: implements %q: %s", curation.ErrInvalid, ref, c.Why)
		}
	}
	existing, err := s.items(ctx, repo, "")
	if err != nil {
		return ProposalView{}, err
	}
	if len(existing) >= curation.MaxItemsPerRepo {
		return ProposalView{}, fmt.Errorf("%w: this repository already has %d proposals and links; withdraw or remove some first", curation.ErrInvalid, len(existing))
	}
	it, err := curation.PrepareProposal(repo, who.Org, who.User, who.Client, in, s.now())
	if err != nil {
		return ProposalView{}, err
	}
	for i, id := range it.Proposal.Supersedes {
		it.Proposal.Supersedes[i] = canonical(l, id)
	}
	for i, id := range it.Proposal.Relates {
		it.Proposal.Relates[i] = canonical(l, id)
	}
	it.Proposal.SuggestedID, it.Proposal.SuggestedPath = l.suggest(it.Proposal.Type, it.Proposal.Title)
	if err := s.Items.SaveCurated(ctx, it); err != nil {
		return ProposalView{}, err
	}
	return s.proposalView(ctx, repo, it, l, true), nil
}

// canonical maps an ID in any letter case to the record's own spelling.
func canonical(l *loaded, id string) string {
	for _, k := range l.order {
		if strings.EqualFold(k, id) {
			return k
		}
	}
	return id
}

// Proposals lists the proposals, newest first, without their bodies.
func (s Service) Proposals(ctx context.Context, repo string) ([]ProposalView, error) {
	l, err := s.load(ctx, repo)
	if err != nil {
		return nil, err
	}
	its, err := s.items(ctx, repo, curation.KindProposal)
	if err != nil {
		return nil, err
	}
	out := make([]ProposalView, 0, len(its))
	for _, it := range its {
		if it.Proposal != nil {
			out = append(out, s.proposalView(ctx, repo, it, l, false))
		}
	}
	return out, nil
}

// Proposal returns one proposal with its body.
func (s Service) Proposal(ctx context.Context, repo, id string) (ProposalView, error) {
	l, err := s.load(ctx, repo)
	if err != nil {
		return ProposalView{}, err
	}
	it, err := s.proposalItem(ctx, repo, id)
	if err != nil {
		return ProposalView{}, err
	}
	return s.proposalView(ctx, repo, it, l, true), nil
}

func (s Service) proposalItem(ctx context.Context, repo, id string) (curation.Item, error) {
	if s.Items == nil {
		return curation.Item{}, ErrDisabled
	}
	it, err := s.Items.GetCurated(ctx, repo, id)
	if err != nil || it.Kind != curation.KindProposal || it.Proposal == nil || it.OrgID != s.Org {
		return curation.Item{}, fmt.Errorf("no such proposal: %w", store.ErrNotFound)
	}
	return it, nil
}

// Withdraw marks a proposal withdrawn. It is kept as history, not deleted.
func (s Service) Withdraw(ctx context.Context, repo, id string) (ProposalView, error) {
	it, err := s.proposalItem(ctx, repo, id)
	if err != nil {
		return ProposalView{}, err
	}
	l, err := s.load(ctx, repo)
	if err != nil {
		return ProposalView{}, err
	}
	it.Proposal.State, it.UpdatedAt = curation.Withdrawn, s.now().UTC().Format(time.RFC3339)
	if err := s.Items.SaveCurated(ctx, it); err != nil {
		return ProposalView{}, err
	}
	return s.proposalView(ctx, repo, it, l, true), nil
}

// Export renders a proposal as the file to commit: front matter with the
// draft status, the title and the body. The ID and path come from the records
// that exist now, so they are right even if others were added since.
func (s Service) Export(ctx context.Context, repo, id string) (filePath, markdown string, err error) {
	v, err := s.Proposal(ctx, repo, id)
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.WriteString("---\n")
	if v.Type != arch.ADR && v.Type != arch.RFC {
		fmt.Fprintf(&b, "id: %s\n", v.SuggestedID)
	}
	fmt.Fprintf(&b, "type: %s\nstatus: %s\n", v.Type, v.Status)
	for _, f := range []struct {
		key  string
		vals []string
	}{{"components", v.Components}, {"labels", v.Labels}, {"supersedes", v.Supersedes}, {"relates", v.Relates}} {
		if len(f.vals) > 0 {
			fmt.Fprintf(&b, "%s: [%s]\n", f.key, strings.Join(f.vals, ", "))
		}
	}
	if v.Topic != "" {
		fmt.Fprintf(&b, "topic: %s\n", v.Topic)
	}
	if len(v.Implements) > 0 {
		refs := make([]string, len(v.Implements))
		for i, c := range v.Implements {
			refs[i] = c.Ref
		}
		fmt.Fprintf(&b, "implements: [%s]\n", strings.Join(refs, ", "))
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "<!-- Proposed through CodeSynapse by %s on %s. Review it like any change; it is not a decision until merged with an accepted status. -->\n", cmp.Or(v.Client, "a user"), strings.SplitN(v.CreatedAt, "T", 2)[0])
	fmt.Fprintf(&b, "# %s\n\n%s\n", v.Title, strings.TrimSpace(v.Body))
	return v.SuggestedPath, b.String(), nil
}

// AddLink records that a piece of code carries out a record, that two records
// relate, or that a record governs a component. The record must exist and an
// implements target must name exactly one place; asking twice returns the
// first link.
func (s Service) AddLink(ctx context.Context, repo string, who Actor, kind, record, target, note string) (LinkView, error) {
	if s.Items == nil {
		return LinkView{}, ErrDisabled
	}
	l, err := s.load(ctx, repo)
	if err != nil {
		return LinkView{}, err
	}
	rec := canonical(l, record)
	if _, ok := l.nodes[rec]; !ok {
		return LinkView{}, fmt.Errorf("%w: %q is not a record in this repository", curation.ErrInvalid, record)
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case curation.Implements:
		if c := s.check(ctx, repo, target); !c.Found {
			return LinkView{}, fmt.Errorf("%w: %s", curation.ErrInvalid, c.Why)
		}
	case curation.Relates:
		t := canonical(l, strings.TrimSpace(target))
		if _, ok := l.nodes[t]; !ok || t == rec {
			return LinkView{}, fmt.Errorf("%w: %q is not another record in this repository", curation.ErrInvalid, target)
		}
		target = t
	}
	existing, err := s.items(ctx, repo, "")
	if err != nil {
		return LinkView{}, err
	}
	if len(existing) >= curation.MaxItemsPerRepo {
		return LinkView{}, fmt.Errorf("%w: this repository already has %d proposals and links", curation.ErrInvalid, len(existing))
	}
	it, err := curation.PrepareLink(repo, who.Org, who.User, who.Client, kind, rec, target, note, s.now())
	if err != nil {
		return LinkView{}, err
	}
	for _, e := range existing {
		if e.Link != nil && e.Link.Kind == it.Link.Kind && e.Link.Record == it.Link.Record && strings.EqualFold(e.Link.Target, it.Link.Target) {
			return s.linkView(ctx, repo, e), nil
		}
	}
	if err := s.Items.SaveCurated(ctx, it); err != nil {
		return LinkView{}, err
	}
	return s.linkView(ctx, repo, it), nil
}

// RemoveLink deletes a link added through the catalog. Relations declared in
// the record files are changed in the files.
func (s Service) RemoveLink(ctx context.Context, repo, id string) error {
	if s.Items == nil {
		return ErrDisabled
	}
	it, err := s.Items.GetCurated(ctx, repo, id)
	if err != nil || it.Kind != curation.KindLink || it.OrgID != s.Org {
		return fmt.Errorf("no such link: %w", store.ErrNotFound)
	}
	return s.Items.DeleteCurated(ctx, repo, id)
}

func (s Service) linkView(ctx context.Context, repo string, it curation.Item) LinkView {
	k := it.Link
	v := LinkView{ID: it.ID, Kind: k.Kind, Record: k.Record, Target: k.Target, Note: k.Note, CreatedAt: it.CreatedAt, CreatedBy: it.CreatedBy, Client: it.Client}
	if k.Kind == curation.Implements {
		n, err := s.resolveCode(ctx, repo, k.Target)
		if err != nil {
			v.Stale, v.Problem = true, err.Error()
		} else {
			v.Resolved = []query.Ref{query.RefOf(n)}
		}
	}
	return v
}

// Links lists the links added through the catalog, optionally for one record
// (either end), with the code each one points at now.
func (s Service) Links(ctx context.Context, repo, record string) ([]LinkView, error) {
	its, err := s.items(ctx, repo, curation.KindLink)
	if err != nil {
		return nil, err
	}
	out := []LinkView{}
	for _, it := range its {
		if it.Link == nil {
			continue
		}
		if record != "" && !strings.EqualFold(it.Link.Record, record) && (it.Link.Kind != curation.Relates || !strings.EqualFold(it.Link.Target, record)) {
			continue
		}
		out = append(out, s.linkView(ctx, repo, it))
	}
	return out, nil
}
