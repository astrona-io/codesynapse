package accounts

import (
	"context"
	"fmt"
	"slices"
)

// MaxAccessGraphs bounds the graphs one access question names.
const MaxAccessGraphs = 50

// AccessQuery asks what a principal holds in one organisation.
type AccessQuery struct {
	OrgID string `json:"org_id"`
	// Graphs limits the answer to these repositories (at most
	// MaxAccessGraphs); empty means every repository the principal reaches.
	Graphs []string `json:"graphs,omitempty"`
	// OrgOnly asks for the organisation role and permissions only.
	OrgOnly bool `json:"org_only,omitempty"`
}

// Validate refuses a query without an organisation or with too many graphs.
func (q AccessQuery) Validate() error {
	if q.OrgID == "" {
		return fmt.Errorf("%w: org_id is required", ErrInvalidInput)
	}
	if len(q.Graphs) > MaxAccessGraphs {
		return fmt.Errorf("%w: at most %d graphs", ErrInvalidInput, MaxAccessGraphs)
	}
	if q.OrgOnly && len(q.Graphs) > 0 {
		return fmt.Errorf("%w: org_only takes no graphs", ErrInvalidInput)
	}
	for _, g := range q.Graphs {
		if g == "" || len(g) > 512 {
			return fmt.Errorf("%w: invalid graph name", ErrInvalidInput)
		}
	}
	return nil
}

// RemoteAccess answers access questions in a process that does not decide
// them itself: workers and the MCP server ask the API, which alone holds the
// OpenFGA key (docs/plans/OPENFGA_PLAN.md, phase 6). It authenticates with
// the caller's own credentials and runtime session, never a service token,
// and any error refuses.
type RemoteAccess interface {
	AccessIn(ctx context.Context, p Principal, q AccessQuery) (Access, error)
}

// AccessIn is AccessFor limited to one organisation and, when q names them,
// to some of its graphs. With Remote set the API answers; the result is
// filtered again here, so an answer about anyone or anything else is never
// used.
func (s *Service) AccessIn(ctx context.Context, p Principal, q AccessQuery) (Access, error) {
	if err := q.Validate(); err != nil {
		return Access{}, err
	}
	if p.OrgID != "" && p.OrgID != q.OrgID {
		return Access{}, ErrNotFound
	}
	var (
		a   Access
		err error
	)
	if s.Remote != nil {
		a, err = s.Remote.AccessIn(ctx, p, q)
		if err == nil && a.UserID != p.User.ID {
			err = fmt.Errorf("%w: the access answer names another user", ErrForbidden)
		}
		if err == nil {
			// The key is read here too, so a key revoked since the API
			// answered never acts on that answer.
			var limit Perms
			if limit, err = s.KeyLimit(ctx, p); err == nil {
				a = a.Within(limit)
			}
		}
	} else {
		a, err = s.accessFor(ctx, p, &q)
	}
	if err != nil {
		return Access{}, err
	}
	return a.In(q), nil
}

// In keeps only what q asks about.
func (a Access) In(q AccessQuery) Access {
	out := Access{UserID: a.UserID, Orgs: map[string]Role{}, OrgPerms: map[string]Perms{}, Teams: map[string]string{}}
	if r, ok := a.Orgs[q.OrgID]; ok {
		out.Orgs[q.OrgID] = r
	}
	if p, ok := a.OrgPerms[q.OrgID]; ok {
		out.OrgPerms[q.OrgID] = p
	}
	if q.OrgOnly {
		return out
	}
	for _, r := range a.Repos {
		if r.OrgID != q.OrgID || (len(q.Graphs) > 0 && !slices.Contains(q.Graphs, r.Graph)) {
			continue
		}
		out.Repos = append(out.Repos, r)
	}
	return out
}
