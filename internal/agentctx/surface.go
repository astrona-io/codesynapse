package agentctx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Attack surface (docs/PLAN.md, security roadmap item 4 and agent workflow
// roadmap step 12): the entry points the code registers (HTTP routes, MCP
// tools, CLI commands; Route nodes) with the guards seen where they are
// registered and the sensitive operations their handlers reach through the
// call graph.

// sinkWeight ranks the security tags worth naming when a handler reaches
// them: running commands, queries and templates first, then decoding
// input (nearly every handler decodes JSON; unsafe formats show up as
// findings), files, network and environment, then weak randomness and TLS
// settings. "http"
// (every handler uses net/http), "crypto" and "reflect" (hashing IDs and
// encoding are everywhere) say nothing about exposure; "auth" is reported
// as a check, not a risk.
var sinkWeight = map[string]int{
	"exec": 3, "sql": 3, "cypher": 3, "template": 3, "unsafe": 3, "syscall": 3,
	"deserialize": 2, "filesystem": 2, "network": 2, "env": 2,
	"weak-crypto": 1, "insecure-rand": 1, "tls": 1,
}

// sinkTags lists sinkWeight's tags for messages.
var sinkTags = func() []string {
	tags := make([]string, 0, len(sinkWeight))
	for t := range sinkWeight {
		tags = append(tags, t)
	}
	slices.Sort(tags)
	return tags
}()

// ErrSurfaceFilter reports an unknown kind or tag filter.
var ErrSurfaceFilter = errors.New("invalid filter")

const (
	authTag = "auth"
	// surfaceDepth is how many calls from the handler a sink may be.
	surfaceDepth = 4
	// maxSurfaceWalks bounds the handlers walked for one answer.
	maxSurfaceWalks = 300
)

// Sink is one sensitive operation an entry point reaches.
type Sink struct {
	Tag string `json:"tag"`
	// Via is the closest function or external call carrying the tag.
	Via   string `json:"via"`
	Depth int    `json:"depth"`
}

// EntryPoint is one registered route, tool or command.
type EntryPoint struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Registered string `json:"registered"`
	Handler    string `json:"handler,omitempty"`
	// InlineHandler is set when the handler is a function literal or a
	// variable at the registration: read it there; its reach is unknown.
	InlineHandler bool     `json:"inline_handler,omitempty"`
	Guards        []string `json:"guards,omitempty"`
	// ChecksAuth is set when the handler reaches an authorization call.
	ChecksAuth bool   `json:"checks_auth,omitempty"`
	Reaches    []Sink `json:"reaches,omitempty"`
}

// AttackSurface lists entry points, most exposed first.
type AttackSurface struct {
	Budget      *ResponseBudget `json:"budget,omitempty"`
	Total       int             `json:"total"`
	ByKind      map[string]int  `json:"by_kind"`
	Unguarded   int             `json:"unguarded"`
	EntryPoints []EntryPoint    `json:"entry_points"`
	Notes       []string        `json:"notes"`
}

// SurfaceOptions narrow an attack-surface answer.
type SurfaceOptions struct {
	// Kind is http, mcp or cli; empty for all.
	Kind string
	// Reaching keeps entry points that reach this tag (exec, sql, …).
	Reaching string
	// Unguarded keeps entry points with no guard at registration and no
	// authorization call in reach.
	Unguarded bool
	// Query keeps entry points whose name contains it.
	Query     string
	Limit     int
	MaxTokens int
}

var surfaceNotes = []string{
	"Guards are what the registration shows: the other arguments and the functions wrapped around the handler. Middleware applied to a whole router or server (r.Use(auth), a wrapping http.Handler) is not seen, and MCP servers usually authenticate the whole connection, so check how the server is built before calling an entry point unprotected.",
	"Reach follows the static call graph from the handler, through interface dispatch, up to 4 calls deep; calls through function values the type checker cannot follow are not included.",
}

// AttackSurface lists repo's entry points with their guards and reach.
func (s Service) AttackSurface(ctx context.Context, repo string, o SurfaceOptions) (AttackSurface, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	switch o.Kind {
	case "", model.RouteHTTP, model.RouteMCP, model.RouteCLI:
	default:
		return AttackSurface{}, fmt.Errorf("%w: kind must be http, mcp or cli, not %q", ErrSurfaceFilter, o.Kind)
	}
	if _, ok := sinkWeight[o.Reaching]; o.Reaching != "" && o.Reaching != authTag && !ok {
		return AttackSurface{}, fmt.Errorf("%w: reaching must be one of %s", ErrSurfaceFilter, strings.Join(sinkTags, ", "))
	}
	s.R = store.ForRepository(s.R, repo)
	routes, err := s.R.List(ctx, repo, store.Filter{Kinds: []model.NodeKind{model.KindRoute}}, store.ByName, store.MaxLimit)
	if err != nil {
		return AttackSurface{}, err
	}
	out := AttackSurface{ByKind: map[string]int{}, EntryPoints: []EntryPoint{}, Notes: slices.Clone(surfaceNotes)}
	if len(routes) == store.MaxLimit {
		out.Notes = append(out.Notes, fmt.Sprintf("Only the first %d entry points are read.", store.MaxLimit))
	}
	walks := 0
	for _, rk := range routes {
		r := rk.Node
		out.ByKind[r.Signature]++
		if o.Kind != "" && r.Signature != o.Kind || o.Query != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(o.Query)) {
			continue
		}
		if walks == maxSurfaceWalks {
			out.Notes = append(out.Notes, fmt.Sprintf("Reach was worked out for the first %d matching entry points; narrow with kind or query.", maxSurfaceWalks))
			break
		}
		walks++
		ep, err := s.entryPoint(ctx, r)
		if err != nil {
			return AttackSurface{}, err
		}
		unguarded := len(ep.Guards) == 0 && !ep.ChecksAuth
		if unguarded {
			out.Unguarded++
		}
		if o.Unguarded && !unguarded || o.Reaching != "" && !reaches(ep, o.Reaching) {
			continue
		}
		out.EntryPoints = append(out.EntryPoints, ep)
	}
	out.Total = len(out.EntryPoints)
	// Most exposed first: unguarded, then reaching more kinds of sinks.
	slices.SortStableFunc(out.EntryPoints, func(a, b EntryPoint) int {
		ua, ub := len(a.Guards) == 0 && !a.ChecksAuth, len(b.Guards) == 0 && !b.ChecksAuth
		if ua != ub {
			if ua {
				return -1
			}
			return 1
		}
		wa, sa := exposure(a)
		wb, sb := exposure(b)
		return cmp.Or(cmp.Compare(wb, wa), cmp.Compare(sb, sa), cmp.Compare(a.Kind+a.Name, b.Kind+b.Name))
	})
	limit := store.ClampLimit(o.Limit, 50)
	if len(out.EntryPoints) > limit {
		out.Notes = append(out.Notes, fmt.Sprintf("%d more entry points match; raise limit or narrow the question.", len(out.EntryPoints)-limit))
		out.EntryPoints = out.EntryPoints[:limit]
	}
	err = fitResponse(&out, &out.Budget, o.MaxTokens, func() int { return trimTail(&out.EntryPoints) })
	return out, err
}

// exposure is the weight of the worst sink an entry point reaches and the
// sum over all of them.
func exposure(ep EntryPoint) (worst, sum int) {
	for _, r := range ep.Reaches {
		w := sinkWeight[r.Tag]
		worst, sum = max(worst, w), sum+w
	}
	return worst, sum
}

func reaches(ep EntryPoint, tag string) bool {
	if tag == authTag {
		return ep.ChecksAuth
	}
	return slices.ContainsFunc(ep.Reaches, func(s Sink) bool { return s.Tag == tag })
}

// entryPoint describes one Route node: its handler, guards and reach.
func (s Service) entryPoint(ctx context.Context, r model.Node) (EntryPoint, error) {
	ep := EntryPoint{Kind: r.Signature, Name: r.Name, Registered: r.Location()}
	if r.Summary != "" {
		ep.Guards = strings.Split(r.Summary, "; ")
	}
	handlers, err := s.R.Traverse(ctx, r.ID, []model.EdgeKind{model.EdgeHandledBy}, store.Out, 1, 1)
	if err != nil {
		return ep, err
	}
	if len(handlers) == 0 {
		ep.InlineHandler = true
		return ep, nil
	}
	h := handlers[0].Node
	ep.Handler = h.ShortName()
	reached, err := s.R.Traverse(ctx, h.ID, []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo}, store.Out, surfaceDepth, store.MaxLimit)
	if err != nil {
		return ep, err
	}
	best := map[string]Sink{}
	note := func(n model.Node, depth int) {
		for _, tag := range n.SecurityTags {
			if tag == authTag {
				ep.ChecksAuth = true
				continue
			}
			if _, ok := sinkWeight[tag]; !ok {
				continue
			}
			// The closest carrier wins; at equal depth an external call
			// (the operation itself) beats a function that makes it.
			cur, seen := best[tag]
			if !seen || depth < cur.Depth || depth == cur.Depth && n.Kind == model.KindExternal {
				best[tag] = Sink{Tag: tag, Via: n.ShortName(), Depth: depth}
			}
		}
	}
	note(h, 0)
	for _, hit := range reached {
		note(hit.Node, hit.Depth)
	}
	for _, sk := range best {
		ep.Reaches = append(ep.Reaches, sk)
	}
	slices.SortFunc(ep.Reaches, func(a, b Sink) int { return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Tag, b.Tag)) })
	return ep, nil
}

// reachedFrom lists the entry points whose handlers lead to id, nearest
// first, and how many there are.
func (s Service) reachedFrom(ctx context.Context, id string, limit int) ([]Ref, int, error) {
	hits, err := s.R.Traverse(ctx, id, []model.EdgeKind{model.EdgeCalls, model.EdgeDispatchesTo, model.EdgeHandledBy}, store.In, surfaceDepth+1, store.MaxLimit)
	if err != nil {
		return nil, 0, err
	}
	var routes []model.Hit
	for _, h := range hits {
		if h.Node.Kind == model.KindRoute {
			routes = append(routes, h)
		}
	}
	slices.SortStableFunc(routes, func(a, b model.Hit) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Node.ShortName(), b.Node.ShortName()))
	})
	if len(routes) == 0 {
		return nil, 0, nil
	}
	return refs(routes, limit, false), len(routes), nil
}
