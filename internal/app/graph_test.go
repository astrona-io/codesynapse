package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// An installation upgraded from the Neo4j era must stop with a clear error,
// not retry forever or start on an empty graph.
func TestNeo4jBackendIsRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f := GraphFlags{Backend: "neo4j"}
	_, err := f.Open(ctx)
	assert.True(t, errors.Is(err, ErrBackendRemoved))
	_, err = f.OpenWithRetry(ctx, func(err error) { t.Errorf("configuration errors are not retried: %v", err) })
	assert.True(t, errors.Is(err, ErrBackendRemoved))
	_, err = (&GraphFlags{Backend: "hugegraph"}).OpenWithRetry(ctx, func(error) { t.Error("not retried") })
	assert.True(t, errors.Is(err, ErrBackendRemoved))
	_, err = (&GraphFlags{Backend: "postgres"}).OpenWithRetry(ctx, func(error) { t.Error("not retried") })
	assert.Error(t, err)
}
