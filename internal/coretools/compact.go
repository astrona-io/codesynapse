package coretools

import (
	"context"
	"encoding/json"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// compact drops the repository from the node IDs in an answer about it
// ("repo|Kind|local" becomes "Kind|local"): the caller named the
// repository already, and the prefix repeats in every ID. It also writes
// the repository's root Go module path as "~" in node IDs and package
// paths ("Function|~/internal/x.F", "GET /x (~/api)"). Every tool that
// takes a symbol accepts the compact form back (agentctx.Resolve). IDs of
// other repositories are left whole.
func compact[T any](ctx context.Context, r store.Reader, repo string, v T, err error) (T, error) {
	if err != nil || repo == "" {
		return v, err
	}
	module := ""
	if n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo)); err == nil {
		module = model.GoModuleOf(n)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	raw, err = agentctx.CompactAnswer(raw, repo, module)
	if err != nil {
		return v, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return v, err
	}
	return out, nil
}
