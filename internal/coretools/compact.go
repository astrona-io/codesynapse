package coretools

import (
	"encoding/json"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// compact drops the repository from the node IDs in an answer about it
// ("repo|Kind|local" becomes "Kind|local"): the caller named the
// repository already, and the prefix repeats in every ID. Every tool that
// takes a symbol accepts the compact form back (agentctx.Resolve). IDs of
// other repositories are left whole.
func compact[T any](repo string, v T, err error) (T, error) {
	if err != nil || repo == "" {
		return v, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	var doc any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber() // keep integers exact through the round trip
	if err := dec.Decode(&doc); err != nil {
		return v, err
	}
	raw, err = json.Marshal(stripRepo(doc, repo+"|"))
	if err != nil {
		return v, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return v, err
	}
	return out, nil
}

func stripRepo(v any, prefix string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = stripRepo(e, prefix)
		}
	case []any:
		for i, e := range x {
			x[i] = stripRepo(e, prefix)
		}
	case string:
		if rest, ok := strings.CutPrefix(x, prefix); ok {
			if kind, local, ok := strings.Cut(rest, "|"); ok && local != "" && model.NodeKind(kind).Valid() {
				return rest
			}
		}
	}
	return v
}
