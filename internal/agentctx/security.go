package agentctx

import (
	"context"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/model"
)

// SecurityContext is what to keep intact when changing a symbol that
// reaches sensitive operations or carries findings (docs/PLAN.md, security
// roadmap item 11): the operations, whether an authorization call is in
// reach, and how the entry points leading here are guarded.
type SecurityContext struct {
	Reaches    []Sink `json:"reaches,omitempty"`
	ChecksAuth bool   `json:"checks_auth,omitempty"`
	// EntryPointsChecked is how many of the nearest entry points had their
	// guards and authorization calls worked out; Unguarded counts those
	// with neither.
	EntryPointsChecked int `json:"entry_points_checked,omitempty"`
	Unguarded          int `json:"unguarded,omitempty"`
	// EntryPoints are the checked ones with their guards (full verbosity).
	EntryPoints []EntryPoint `json:"entry_points,omitempty"`
	Note        string       `json:"note,omitempty"`
}

// securityOf builds the security context of n, or nil when n reaches no
// sensitive operation and has no findings. routes are the entry points
// leading to n, nearest first; at most limit of them are walked.
func (s Service) securityOf(ctx context.Context, n model.Node, routes []model.Hit, findings, limit int, full bool) (*SecurityContext, error) {
	if !n.Kind.IsCallable() {
		return nil, nil
	}
	sinks, checksAuth, err := s.sinksFrom(ctx, n)
	if err != nil || len(sinks) == 0 && findings == 0 {
		return nil, err
	}
	sc := &SecurityContext{Reaches: sinks, ChecksAuth: checksAuth}
	for _, r := range routes[:min(len(routes), limit)] {
		ep, err := s.entryPoint(ctx, r.Node)
		if err != nil {
			return nil, err
		}
		sc.EntryPointsChecked++
		if len(ep.Guards) == 0 && !ep.ChecksAuth {
			sc.Unguarded++
		}
		if full {
			// What the handler reaches is in attack_surface; here it is
			// mostly this symbol's reach again.
			ep.Reaches = nil
			sc.EntryPoints = append(sc.EntryPoints, ep)
		}
	}
	switch {
	case sc.Unguarded > 0:
		sc.Note = fmt.Sprintf("%d of %d entry points checked reach this with no guard at registration and no authorization call: keep input validation here and do not widen what callers can make it do.", sc.Unguarded, sc.EntryPointsChecked)
	case len(routes) > sc.EntryPointsChecked:
		sc.Note = fmt.Sprintf("Guards were checked for the nearest %d of %d entry points; attack_surface lists the rest.", sc.EntryPointsChecked, len(routes))
	}
	return sc, nil
}
