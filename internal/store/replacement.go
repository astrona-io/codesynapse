package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/model"
)

// MaxReplacementBytes bounds the graph staged for an atomic repository replacement.
const MaxReplacementBytes = 64 << 20

// Replacement validates the whole graph before a backend changes visible data.
// Dangling edges are discarded, matching in-memory graph extraction semantics.
func Replacement(ctx context.Context, repo string, g *model.Graph) ([]model.Node, []model.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if g == nil {
		return nil, nil, errors.New("replacement graph is required")
	}
	nodes := g.Nodes()
	edges := g.Edges()
	ids := make(map[string]bool, len(nodes))
	size := 0
	measure := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		size += len(b)
		if size > MaxReplacementBytes {
			return errors.New("repository replacement exceeds 64 MiB")
		}
		return ctx.Err()
	}
	for _, n := range nodes {
		if n.Repo != repo {
			return nil, nil, errors.New("replacement node belongs to another repository")
		}
		if !n.Kind.Valid() {
			return nil, nil, errors.New("replacement contains an invalid node kind")
		}
		if err := measure(n); err != nil {
			return nil, nil, fmt.Errorf("validate replacement node: %w", err)
		}
		ids[n.ID] = true
	}
	valid := make([]model.Edge, 0, len(edges))
	for _, e := range edges {
		if !e.Kind.Valid() {
			return nil, nil, errors.New("replacement contains an invalid edge kind")
		}
		if !e.Evidence.Valid() {
			return nil, nil, errors.New("replacement contains invalid edge evidence")
		}
		if err := measure(e); err != nil {
			return nil, nil, fmt.Errorf("validate replacement edge: %w", err)
		}
		if ids[e.From] && ids[e.To] {
			valid = append(valid, e)
		}
	}
	return nodes, valid, nil
}

// ReadEvidence converts a stored evidence value. Values this version does not
// know, including those written by newer versions, read as unknown: never
// as stronger evidence than was proven.
func ReadEvidence(s string) model.Evidence {
	if e := model.Evidence(s); e.Valid() {
		return e
	}
	return ""
}
