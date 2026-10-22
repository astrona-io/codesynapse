package catalog

import (
	"context"
	"fmt"
	"slices"

	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/identity"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// CodeView keeps historical evidence separate from current freshness.
type CodeView struct {
	Item      curation.Item `json:"item"`
	Freshness string        `json:"freshness"`
	Changed   []string      `json:"changed"`
}

// CodePage reports storage-window omissions, including when a bounded store
// query cannot prove that it has seen every organisation's items.
type CodePage struct {
	Records          []CodeView `json:"records"`
	Truncated        bool       `json:"truncated"`
	Limit            int        `json:"limit"`
	Offset           int        `json:"offset"`
	Total            int        `json:"total"`
	Omitted          int        `json:"omitted"`
	StorageTruncated bool       `json:"storage_truncated"`
}

func sourcesOf(ids []string, r identity.Report, active bool) ([]curation.SymbolSource, error) {
	out := []curation.SymbolSource{}
	for _, id := range ids {
		found := false
		for _, s := range r.Symbols {
			if s.ID == id && (!active || s.Active) {
				out = append(out, curation.SymbolSource{ID: id, Revision: s.Current.Revision, NodeID: s.Current.NodeID, Fingerprint: s.Current.Fingerprint})
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: symbol %s is not available in this repository (destinations must be current)", curation.ErrInvalid, id)
		}
	}
	return out, nil
}

// RecordCode stores explicit provenance or an annotation. It cannot alter
// symbol matching, code, approval states, or propagate any change.
func (s Service) RecordCode(ctx context.Context, repo string, who Actor, in curation.CodeInput) (CodeView, error) {
	if s.Items == nil {
		return CodeView{}, ErrDisabled
	}
	if who.Org != s.Org {
		return CodeView{}, fmt.Errorf("%w: organisation mismatch", curation.ErrInvalid)
	}
	it, err := curation.PrepareCode(repo, who.Org, who.User, who.Client, in, s.now())
	if err != nil {
		return CodeView{}, err
	}
	r, available, err := query.IdentityReport(ctx, s.R, repo)
	if err != nil {
		return CodeView{}, err
	}
	if !available {
		return CodeView{}, fmt.Errorf("%w: sync this repository to initialise symbol identities", curation.ErrInvalid)
	}
	it.Code.Sources, err = sourcesOf(it.Code.Origins, r, false)
	if err != nil {
		return CodeView{}, err
	}
	it.Code.Targets, err = sourcesOf(it.Code.Destinations, r, true)
	if err != nil {
		return CodeView{}, err
	}
	all, err := s.Items.ListCurated(ctx, repo, "", store.MaxLimit)
	if err != nil {
		return CodeView{}, err
	}
	if len(all) >= store.MaxLimit {
		return CodeView{}, fmt.Errorf("%w: curated item limit reached", curation.ErrInvalid)
	}
	if err := s.Items.SaveCurated(ctx, it); err != nil {
		return CodeView{}, err
	}
	return codeView(it, indexSymbols(r)), nil
}

func indexSymbols(r identity.Report) map[string]identity.Symbol {
	index := make(map[string]identity.Symbol, len(r.Symbols))
	for _, s := range r.Symbols {
		index[s.ID] = s
	}
	return index
}

func codeView(it curation.Item, index map[string]identity.Symbol) CodeView {
	v := CodeView{Item: it, Freshness: "current", Changed: []string{}}
	for _, saved := range append(slices.Clone(it.Code.Sources), it.Code.Targets...) {
		state := "missing"
		if current, ok := index[saved.ID]; ok {
			switch {
			case !current.Active:
				state = "removed"
			case saved.Fingerprint == "" || current.Current.Fingerprint == "":
				state = "unknown"
			case current.Current.Fingerprint != saved.Fingerprint:
				state = "changed"
			default:
				state = "current"
			}
		}
		if state != "current" {
			v.Changed = append(v.Changed, saved.ID+": "+state)
			v.Freshness = "needs review"
		}
	}
	return v
}

// CodeRecords returns only this organisation's evidence. All endpoints and
// tools use it, so shared code graphs never share private annotations.
func (s Service) CodeRecords(ctx context.Context, repo, symbol string, offset, limit int) (CodePage, error) {
	page := CodePage{Records: []CodeView{}, Limit: min(store.ClampLimit(limit, 20), 100), Offset: max(offset, 0)}
	if s.Items == nil {
		return page, ErrDisabled
	}
	r, _, err := query.IdentityReport(ctx, s.R, repo)
	if err != nil {
		return page, err
	}
	all, err := s.Items.ListCurated(ctx, repo, curation.KindCode, store.MaxLimit)
	if err != nil {
		return page, err
	}
	page.StorageTruncated = len(all) >= store.MaxLimit
	matches := []curation.Item{}
	for _, it := range all {
		if it.OrgID != s.Org || it.Code == nil {
			continue
		}
		if symbol != "" && !slices.Contains(it.Code.Origins, symbol) && !slices.Contains(it.Code.Destinations, symbol) {
			continue
		}
		matches = append(matches, it)
	}
	page.Total = len(matches)
	start := min(page.Offset, len(matches))
	end := min(start+page.Limit, len(matches))
	index := indexSymbols(r)
	for _, it := range matches[start:end] {
		page.Records = append(page.Records, codeView(it, index))
	}
	page.Omitted = page.Total - len(page.Records)
	page.Truncated = page.Omitted > 0 || page.StorageTruncated
	return page, nil
}
