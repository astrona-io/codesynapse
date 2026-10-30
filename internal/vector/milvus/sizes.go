package milvus

import (
	"context"
	"fmt"
	"strings"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Store size kinds: the collection families this index owns. Collection
// names carry a model hash, so sizes are summed per family, never named.
const (
	KindVectors        = "vectors"
	KindSessionVectors = "session_vectors"
)

// maxSizedCollections bounds the get_stats calls of one sample.
const maxSizedCollections = 256

func init() {
	telemetry.AllowStoreKinds(telemetry.DBSystemMilvus, KindVectors, KindSessionVectors)
}

// StoreSizes counts the entities of this index's collections, durable and
// session (store.Sizer). Milvus reports no bytes through its API.
func (x *Index) StoreSizes(ctx context.Context) ([]telemetry.StoreSize, error) {
	names, err := x.names(ctx)
	if err != nil {
		return nil, fmt.Errorf("milvus sizes: %w", err)
	}
	var vectors, sessions int64
	sized := 0
	for _, name := range names {
		var n *int64
		switch {
		case strings.HasPrefix(name, x.sessions.prefix):
			n = &sessions
		case strings.HasPrefix(name, x.prefix):
			n = &vectors
		default:
			continue
		}
		if sized++; sized > maxSizedCollections {
			return nil, fmt.Errorf("milvus sizes: more than %d collections", maxSizedCollections)
		}
		var st struct {
			RowCount int64 `json:"rowCount"`
		}
		if err := x.call(ctx, "collections/get_stats", map[string]any{"collectionName": name}, &st); err != nil {
			if gone(err) {
				continue // dropped since the listing
			}
			return nil, fmt.Errorf("milvus sizes: %w", err)
		}
		*n += max(st.RowCount, 0)
	}
	return []telemetry.StoreSize{
		{System: telemetry.DBSystemMilvus, Kind: KindVectors, Rows: vectors, Bytes: -1},
		{System: telemetry.DBSystemMilvus, Kind: KindSessionVectors, Rows: sessions, Bytes: -1},
	}, nil
}
