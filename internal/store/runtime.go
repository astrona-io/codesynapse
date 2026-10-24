package store

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
)

// RuntimeReader is implemented by stores that keep measured runtime facts
// (coverage, test results; internal/runtimefacts) apart from the code
// graph, so a re-index keeps them. Facts are kept per namespace: the graph
// name, or "org|graph" (memory.Namespace) when organisations share a graph,
// so one organisation's uploads never change another's answers.
type RuntimeReader interface {
	// RuntimeFacts returns ErrNotFound when the namespace has none.
	RuntimeFacts(ctx context.Context, ns string) (runtimefacts.Facts, error)
}

// RuntimeStore also writes them.
type RuntimeStore interface {
	RuntimeReader
	SetRuntimeFacts(ctx context.Context, ns string, f runtimefacts.Facts) error
	DeleteRuntimeFacts(ctx context.Context, ns string) error
}

type runtimeScopeKey struct{}

// WithRuntimeScope makes RuntimeFactsOf read ns: the caller's organisation
// namespace for the graph, set by the server from the authenticated caller,
// never from request arguments.
func WithRuntimeScope(ctx context.Context, ns string) context.Context {
	return context.WithValue(ctx, runtimeScopeKey{}, ns)
}

// RuntimeGraph is the graph a namespace belongs to (graph names never
// contain "|").
func RuntimeGraph(ns string) string {
	if i := strings.LastIndex(ns, "|"); i >= 0 {
		return ns[i+1:]
	}
	return ns
}

// RuntimeFactsOf returns the measured facts for repo in the context's scope;
// ok is false when there are none or r keeps none. A scope for another
// graph, or a reader fenced to another repository, finds nothing.
func RuntimeFactsOf(ctx context.Context, r Reader, repo string) (facts runtimefacts.Facts, ok bool, err error) {
	ns := repo
	if scoped, isSet := ctx.Value(runtimeScopeKey{}).(string); isSet && scoped != "" {
		ns = scoped
	}
	if RuntimeGraph(ns) != repo {
		return runtimefacts.Facts{}, false, nil
	}
	if fenced, isFenced := r.(repositoryReader); isFenced {
		if !fenced.allows(repo) {
			return runtimefacts.Facts{}, false, nil
		}
		r = fenced.reader
	}
	rr, isRuntime := r.(RuntimeReader)
	if !isRuntime {
		return runtimefacts.Facts{}, false, nil
	}
	facts, err = rr.RuntimeFacts(ctx, ns)
	if errors.Is(err, ErrNotFound) {
		return runtimefacts.Facts{}, false, nil
	}
	return facts, err == nil, err
}

// RuntimeSource maps reports onto repo's graph in r, asking by path.
func RuntimeSource(r Reader, repo string) runtimefacts.Source {
	return runtimeSource{r: ForRepository(r, repo), repo: repo}
}

type runtimeSource struct {
	r    Reader
	repo string
}

func (s runtimeSource) HasFile(ctx context.Context, file string) (bool, error) {
	_, err := s.r.Node(ctx, model.NodeID(s.repo, model.KindFile, file))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s runtimeSource) FilesIn(ctx context.Context, dir string) ([]string, error) {
	prefix := dir + "/"
	if dir == "." {
		prefix = ""
	}
	ranked, err := s.r.List(ctx, s.repo, Filter{Kinds: []model.NodeKind{model.KindFile}, FilePrefix: prefix}, ByName, MaxLimit)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rk := range ranked {
		if path.Dir(rk.Node.File) == dir {
			out = append(out, rk.Node.File)
		}
	}
	return out, nil
}

func (s runtimeSource) InFile(ctx context.Context, file string) ([]model.Node, error) {
	ranked, err := s.r.List(ctx, s.repo, Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, FilePrefix: file}, ByName, MaxLimit)
	if err != nil {
		return nil, err
	}
	var out []model.Node
	for _, rk := range ranked {
		if rk.Node.File == file {
			out = append(out, rk.Node)
		}
	}
	return out, nil
}
