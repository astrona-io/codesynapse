package accounts

import (
	"context"
	"errors"
	"log/slog"
	"slices"
)

// ErrPolicyStale means the decision service has not caught up with an
// organisation's policy yet. It refuses; the API answers 503 with a short
// Retry-After.
var ErrPolicyStale = errors.New("openfga: organisation policy not synced yet")

// Decider decides permissions in the API once OpenFGA is the decision
// service (docs/plans/OPENFGA_PLAN.md, phase 6). Go still checks the
// bootstrap root, suspension, membership, the API key's organisation and
// its scope in front of it; the Decider answers what the policy grants.
type Decider interface {
	Decide(ctx context.Context, q DecisionRequest) (Decision, error)
}

// DecisionRequest names a user, the organisations they are an active
// member of in PostgreSQL and the usable repository entries of those
// organisations to decide.
type DecisionRequest struct {
	UserID string
	Orgs   []Org
	Repos  []RepoRef
}

// RepoRef is one organisation's entry for a graph.
type RepoRef struct {
	OrgID, Graph string
}

// Decision is what the decision service holds for a request.
type Decision struct {
	// Orgs has one entry per requested organisation the service is current
	// for. A missing organisation is stale.
	Orgs map[string]OrgDecision
	// Repos are the permissions on each requested repository, keyed by
	// RepoObject; only repositories of organisations in Orgs appear.
	Repos map[string]Perms
}

// OrgDecision is the user's standing in one organisation.
type OrgDecision struct {
	// Active: a member or guest the service knows.
	Active bool
	// Role is the strongest level held (owner, full, write, read), Guest
	// for an active member with none.
	Role  Role
	Perms Perms
}

// decide replaces the permissions of a, computed from the PostgreSQL
// snapshot, with the Decider's. Only organisations a lists (active
// memberships) and usable entries of them are asked about, so OpenFGA
// never adds a member or a repository PostgreSQL does not have. With only,
// the question is about that organisation and its stale answer is
// ErrPolicyStale; otherwise a stale organisation is left out, which also
// refuses. graphs narrows the repositories asked about; orgOnly asks none.
func (s *Service) decide(ctx context.Context, a Access, snap policySnapshot, only string, graphs []string, orgOnly bool) (Access, error) {
	req := DecisionRequest{UserID: a.UserID}
	for id := range a.Orgs {
		if only != "" && id != only {
			continue
		}
		org, ok := snap.orgs[id]
		if !ok {
			continue
		}
		req.Orgs = append(req.Orgs, org)
	}
	slices.SortFunc(req.Orgs, func(x, y Org) int { return compareStrings(x.ID, y.ID) })
	asked := map[string]bool{}
	for _, o := range req.Orgs {
		asked[o.ID] = true
	}
	entries := map[string]Repository{}
	if !orgOnly {
		for _, r := range snap.repos {
			if !asked[r.OrgID] || len(graphs) > 0 && !slices.Contains(graphs, r.Graph) {
				continue
			}
			key := RepoObject(r.OrgID, r.Graph)
			if _, dup := entries[key]; dup {
				continue
			}
			entries[key] = r
			req.Repos = append(req.Repos, RepoRef{OrgID: r.OrgID, Graph: r.Graph})
		}
	}
	out := Access{UserID: a.UserID, Orgs: map[string]Role{}, OrgPerms: map[string]Perms{}, Teams: a.Teams}
	if len(req.Orgs) == 0 {
		return out, nil
	}
	d, err := s.Decider.Decide(ctx, req)
	if err != nil {
		return Access{}, err
	}
	for _, o := range req.Orgs {
		od, ok := d.Orgs[o.ID]
		if !ok {
			if o.ID == only {
				return Access{}, ErrPolicyStale
			}
			continue
		}
		if !od.Active {
			continue
		}
		out.Orgs[o.ID] = od.Role
		out.OrgPerms[o.ID] = od.Perms
	}
	// The Go evaluation explains a grant (Via, CustomRole); OpenFGA decides it.
	explained := map[string]RepoAccess{}
	for _, r := range a.Repos {
		explained[RepoObject(r.OrgID, r.Graph)] = r
	}
	disagree := 0
	for _, ref := range req.Repos {
		key := RepoObject(ref.OrgID, ref.Graph)
		if _, ok := out.Orgs[ref.OrgID]; !ok {
			continue
		}
		perms := d.Repos[key]
		why, known := explained[key]
		if known && why.Perms != perms || !known && perms.Has(RepoCodeRead) {
			disagree++
		}
		if !perms.Has(RepoCodeRead) {
			continue
		}
		if !known {
			why = RepoAccess{Via: "openfga"}
		}
		repo := entries[key]
		out.Repos = append(out.Repos, RepoAccess{OrgID: ref.OrgID, Graph: ref.Graph, Role: BuiltinRepoRole(perms), Perms: perms,
			CustomRole: why.CustomRole, Via: why.Via, Revision: snap.orgs[ref.OrgID].PolicyRevision, Personal: repo.Personal()})
	}
	if disagree > 0 {
		// Both read the same revision, so a difference is a sync or model
		// bug: OpenFGA's answer stands, and an operator should look.
		slog.WarnContext(ctx, "openfga and the postgres evaluation disagree", "repositories", disagree)
	}
	return out, nil
}

// policySnapshot is what accessOf read besides the answer: the user's
// organisations and their usable repository entries.
type policySnapshot struct {
	orgs  map[string]Org
	repos []Repository
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
