package catalog

import (
	"context"
	"errors"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

// ErrIdentityConflict refuses to choose between competing human reviews.
var ErrIdentityConflict = errors.New("conflicting identity reviews; a person must review them again")

// IdentityReviewView shows historical assertions apart from effective links.
type IdentityReviewView struct {
	Item  curation.Item `json:"item"`
	State string        `json:"state"`
}

// IdentityReviewPage is bounded and uses only the selected organisation.
type IdentityReviewPage struct {
	Repo      string               `json:"repo"`
	Revision  string               `json:"revision,omitempty"`
	IndexedAt string               `json:"indexed_at,omitempty"`
	Available bool                 `json:"available"`
	Reviews   []IdentityReviewView `json:"reviews"`
	Total     int                  `json:"total"`
	Offset    int                  `json:"offset"`
	Omitted   int                  `json:"omitted"`
	Truncated bool                 `json:"truncated"`
}

func (s Service) identityReviews(ctx context.Context, repo string) ([]curation.Item, map[string]bool, map[string]int, map[string]int, error) {
	if s.Items == nil {
		return nil, nil, nil, nil, nil
	}
	all, err := s.Items.ListCurated(ctx, repo, curation.KindIdentityReview, store.MaxLimit)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(all) >= store.MaxLimit {
		return nil, nil, nil, nil, fmt.Errorf("identity review storage window is incomplete")
	}
	items := []curation.Item{}
	known := map[string]bool{}
	for _, it := range all {
		if it.OrgID == s.Org && it.IdentityReview != nil {
			items = append(items, it)
			known[it.ID] = true
		}
	}
	superseded := map[string]bool{}
	for _, it := range items {
		for _, id := range it.IdentityReview.Supersedes {
			if !known[id] || id == it.ID {
				return nil, nil, nil, nil, fmt.Errorf("invalid identity review history")
			}
			superseded[id] = true
		}
	}
	report, _, err := query.IdentityReport(ctx, s.R, repo)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	visible := indexSymbols(report)
	paths := []string{}
	for _, it := range items {
		paths = append(paths, it.IdentityReview.SourceFile, it.IdentityReview.TargetFile)
	}
	files, err := query.IdentityFileVisibility(ctx, s.R, repo, paths)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	allowed := []curation.Item{}
	for _, it := range items {
		_, fromOK := visible[it.IdentityReview.From]
		_, toOK := visible[it.IdentityReview.To]
		if fromOK && toOK && files[it.IdentityReview.SourceFile] && files[it.IdentityReview.TargetFile] {
			allowed = append(allowed, it)
		}
	}
	items = allowed
	from, to := map[string]int{}, map[string]int{}
	for _, it := range items {
		if !superseded[it.ID] {
			from[it.IdentityReview.From]++
			to[it.IdentityReview.To]++
		}
	}
	return items, superseded, from, to, nil
}

// ReviewIdentity is called only by the human API route, which requires a
// signed-in session and repository administration. There is no MCP write.
func (s Service) ReviewIdentity(ctx context.Context, repo string, who Actor, in curation.IdentityReviewInput) (IdentityReviewView, error) {
	if s.Items == nil {
		return IdentityReviewView{}, ErrDisabled
	}
	if who.Org != s.Org {
		return IdentityReviewView{}, fmt.Errorf("%w: organisation mismatch", curation.ErrInvalid)
	}
	it, err := curation.PrepareIdentityReview(repo, who.Org, who.User, in, s.now())
	if err != nil {
		return IdentityReviewView{}, err
	}
	r, available, err := query.IdentityReport(ctx, s.R, repo)
	if err != nil {
		return IdentityReviewView{}, err
	}
	if !available || in.Revision != r.Revision || in.IndexedAt != r.IndexedAt {
		return IdentityReviewView{}, fmt.Errorf("%w: identity index changed; reload before reviewing", curation.ErrInvalid)
	}
	index := indexSymbols(r)
	source, sourceOK := index[in.From]
	target, targetOK := index[in.To]
	if !sourceOK || !targetOK || !target.Active || source.Kind != target.Kind || source.Language != target.Language {
		return IdentityReviewView{}, fmt.Errorf("%w: source and current target must be compatible symbols in this repository", curation.ErrInvalid)
	}
	if in.Relation == "same-symbol" && source.Active {
		return IdentityReviewView{}, fmt.Errorf("%w: two current declarations cannot share an identity; record a copy instead", curation.ErrInvalid)
	}
	sources, err := sourcesOf([]string{in.From}, r, false)
	if err != nil {
		return IdentityReviewView{}, err
	}
	targets, err := sourcesOf([]string{in.To}, r, true)
	if err != nil {
		return IdentityReviewView{}, err
	}
	it.IdentityReview.Source = sources[0]
	it.IdentityReview.Target = targets[0]
	it.IdentityReview.SourceFile = source.Current.File
	it.IdentityReview.TargetFile = target.Current.File
	previous, superseded, _, _, err := s.identityReviews(ctx, repo)
	if err != nil {
		return IdentityReviewView{}, err
	}
	for _, old := range previous {
		review := old.IdentityReview
		if !superseded[old.ID] && (review.From == in.From || review.To == in.To) {
			it.IdentityReview.Supersedes = append(it.IdentityReview.Supersedes, old.ID)
		}
	}
	if len(it.IdentityReview.Supersedes) > 20 {
		return IdentityReviewView{}, fmt.Errorf("%w: too many competing identity reviews", curation.ErrInvalid)
	}
	all, err := s.Items.ListCurated(ctx, repo, "", store.MaxLimit)
	if err != nil {
		return IdentityReviewView{}, err
	}
	if len(all) >= store.MaxLimit {
		return IdentityReviewView{}, fmt.Errorf("%w: curated item limit reached", curation.ErrInvalid)
	}
	if s.BeforeIdentityReview != nil {
		if err := s.BeforeIdentityReview(ctx, repo); err != nil {
			return IdentityReviewView{}, err
		}
	}
	if err := s.Items.SaveCurated(ctx, it); err != nil {
		return IdentityReviewView{}, err
	}
	return IdentityReviewView{Item: it, State: "active"}, nil
}

// IdentityReviews reads assertions, their supersession and competing heads.
func (s Service) IdentityReviews(ctx context.Context, repo, symbol string, offset, limit int) (IdentityReviewPage, error) {
	page := IdentityReviewPage{Repo: repo, Reviews: []IdentityReviewView{}, Offset: max(offset, 0)}
	if s.Items == nil {
		return page, ErrDisabled
	}
	report, available, err := query.IdentityReport(ctx, s.R, repo)
	page.Available = available
	page.Revision = report.Revision
	page.IndexedAt = report.IndexedAt
	if err != nil {
		return page, err
	}
	items, superseded, from, to, err := s.identityReviews(ctx, repo)
	if err != nil {
		return page, err
	}
	matches := []IdentityReviewView{}
	for _, it := range items {
		review := it.IdentityReview
		if symbol != "" && symbol != review.From && symbol != review.To {
			continue
		}
		state := "active"
		if superseded[it.ID] {
			state = "superseded"
		} else if from[review.From] > 1 || to[review.To] > 1 {
			state = "conflict"
		}
		matches = append(matches, IdentityReviewView{Item: it, State: state})
	}
	page.Total = len(matches)
	start := min(page.Offset, len(matches))
	end := min(start+min(store.ClampLimit(limit, 20), 100), len(matches))
	page.Reviews = matches[start:end]
	page.Omitted = len(matches) - len(page.Reviews)
	page.Truncated = page.Omitted > 0
	return page, nil
}

// ResolveSymbol follows only unambiguous, organisation-scoped same-symbol
// reviews after ordinary resolution fails. Copies never become aliases.
func (s Service) ResolveSymbol(ctx context.Context, repo, symbol string) (model.Node, error) {
	n, err := (query.Service{R: s.R}).Resolve(ctx, repo, symbol)
	if err == nil || !query.IsSymbolID(symbol) || !errors.Is(err, store.ErrNotFound) {
		return n, err
	}
	items, superseded, from, to, loadErr := s.identityReviews(ctx, repo)
	if loadErr != nil {
		return model.Node{}, loadErr
	}
	seen := map[string]bool{}
	for range store.MaxDepth {
		if seen[symbol] {
			return model.Node{}, ErrIdentityConflict
		}
		seen[symbol] = true
		next := ""
		for _, it := range items {
			r := it.IdentityReview
			if superseded[it.ID] || r.From != symbol {
				continue
			}
			if from[r.From] > 1 || to[r.To] > 1 {
				return model.Node{}, ErrIdentityConflict
			}
			if r.Relation == "same-symbol" {
				next = r.To
			}
		}
		if next == "" {
			return model.Node{}, err
		}
		n, resolveErr := (query.Service{R: s.R}).Resolve(ctx, repo, next)
		if resolveErr == nil {
			return n, nil
		}
		if !errors.Is(resolveErr, store.ErrNotFound) {
			return model.Node{}, resolveErr
		}
		symbol = next
	}
	return model.Node{}, fmt.Errorf("identity review chain exceeds depth %d", store.MaxDepth)
}
