package ingest

import (
	"context"
	"errors"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// IdentityBaseline reads the prior state before a serialized repository sync.
// Only a genuinely new repository is an empty baseline; read failures must
// never silently replace existing symbol identities.
func IdentityBaseline(ctx context.Context, r store.Reader, repo string) (string, error) {
	n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading symbol identity baseline: %w", err)
	}
	return n.Identities, nil
}
