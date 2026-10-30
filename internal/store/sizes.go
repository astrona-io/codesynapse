package store

import (
	"context"
	"errors"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// ErrSizesBusy means a repository is being published, so sizes are not
// sampled now: counting while a replace writes in place would count both
// runs, and the count job competes with the publication for storage.
var ErrSizesBusy = errors.New("a publication holds a replace lease")

// Sizer is a store that reports how many rows (and, where the database
// knows, bytes) it keeps per kind (docs/plans/PLATFORM_STATS_PLAN.md, phase 6).
// Kinds come from the store's own schema, never from stored data, and
// statements are constants. It answers ErrSizesBusy while any replace lease
// is held. Sampling is platform work: it reads no tenant data.
type Sizer interface {
	StoreSizes(ctx context.Context) ([]telemetry.StoreSize, error)
}
