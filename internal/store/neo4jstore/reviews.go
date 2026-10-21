package neo4jstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/parisnakitakejser/codesynapse/internal/reviews"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Reviews are :Review nodes (not :Node), so repository replacement and
// deletion, which match :Node, never touch them. The review is stored as one
// JSON document; repo and created_at are indexed for listing.

// SaveReview implements store.ReviewStore.
func (s *Store) SaveReview(ctx context.Context, r reviews.Review) error {
	doc, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.write(ctx, `MERGE (r:Review {id: $id})
SET r.repo = $repo, r.created_at = $created, r.updated_at = $updated, r.doc = $doc`,
		map[string]any{"id": r.ID, "repo": r.Repo, "created": r.CreatedAt, "updated": r.UpdatedAt, "doc": string(doc)})
	return err
}

// GetReview implements store.ReviewStore.
func (s *Store) GetReview(ctx context.Context, id string) (reviews.Review, error) {
	res, err := s.read(ctx, "MATCH (r:Review {id: $id}) RETURN r.doc AS doc", map[string]any{"id": id})
	if err != nil {
		return reviews.Review{}, err
	}
	if len(res.Records) == 0 {
		return reviews.Review{}, fmt.Errorf("review %q: %w", id, store.ErrNotFound)
	}
	var r reviews.Review
	err = json.Unmarshal([]byte(str(res.Records[0], "doc")), &r)
	return r, err
}

// ListReviews implements store.ReviewStore.
func (s *Store) ListReviews(ctx context.Context, repo string, limit int) ([]reviews.Review, error) {
	res, err := s.read(ctx, `MATCH (r:Review {repo: $repo}) RETURN r.doc AS doc ORDER BY r.created_at DESC, r.id LIMIT $limit`,
		map[string]any{"repo": repo, "limit": store.ClampLimit(limit, 50)})
	if err != nil {
		return nil, err
	}
	out := make([]reviews.Review, 0, len(res.Records))
	for _, rec := range res.Records {
		var r reviews.Review
		if err := json.Unmarshal([]byte(str(rec, "doc")), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteReview implements store.ReviewStore.
func (s *Store) DeleteReview(ctx context.Context, id string) error {
	_, err := s.write(ctx, "MATCH (r:Review {id: $id}) DELETE r", map[string]any{"id": id})
	return err
}
