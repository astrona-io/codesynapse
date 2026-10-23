package accounts

import (
	"context"
	"errors"
	"fmt"
)

func policyChange[T any](ctx context.Context, s *Service, fn func(*Service) (T, error)) (T, error) {
	var result T
	events := []Event{}
	err := s.St.PolicyTransaction(ctx, "", func(st Store) error {
		scoped := *s
		scoped.St = st
		scoped.policy = true
		scoped.policyErr = nil
		scoped.Sink = func(e Event) { events = append(events, e) }
		scoped.policyOrgs = map[string]bool{}
		var err error
		result, err = fn(&scoped)
		if err != nil {
			return err
		}
		if scoped.policyErr != nil {
			return scoped.policyErr
		}
		for orgID := range scoped.policyOrgs {
			if err := st.TouchPolicy(ctx, orgID); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && s.Sink != nil {
		for _, e := range events {
			s.Sink(e)
		}
	}
	return result, err
}

// SetDefaultRepoAccess changes the fallback for enrolled, non-guest members.
func (s *Service) SetDefaultRepoAccess(ctx context.Context, p Principal, orgID, policy string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if p.Via == "key" {
			return struct{}{}, ErrForbidden
		}
		if err := tx.Require(ctx, p, orgID, Admin); err != nil {
			return struct{}{}, err
		}
		if err := tx.St.SetDefaultRepoAccess(ctx, orgID, policy); err != nil {
			return struct{}{}, err
		}
		e := actorEvent(p, "org.repo_policy.set", orgID, orgID)
		e.Detail = policy
		tx.audit(ctx, e)
		return struct{}{}, nil
	})
	return err
}

// RecoverRepoAdmin explicitly assigns an existing member to an orphaned
// repository. Organisation ownership grants this recovery action, not code access.
func (s *Service) RecoverRepoAdmin(ctx context.Context, p Principal, orgID, graph, userID string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if p.Via != "session" || (p.OrgID != "" && p.OrgID != orgID) {
			return struct{}{}, ErrForbidden
		}
		role, err := tx.St.MemberRole(ctx, orgID, p.User.ID)
		if err != nil || role != Owner {
			return struct{}{}, ErrForbidden
		}
		if err := tx.ValidatePrincipal(ctx, p); err != nil {
			return struct{}{}, err
		}
		if !tx.orgHasRepo(ctx, orgID, graph) {
			return struct{}{}, ErrNotFound
		}
		if _, err := tx.St.MemberRole(ctx, orgID, userID); err != nil {
			return struct{}{}, ErrForbidden
		}
		u, err := tx.St.UserByID(ctx, userID)
		if err != nil || u.DisabledAt != nil {
			return struct{}{}, ErrForbidden
		}
		repaired, err := tx.repairRepoGrants(ctx, p, orgID, graph)
		if err != nil {
			return struct{}{}, err
		}
		t := Tuple{ObjectType: TypeRepository, ObjectID: RepoObject(orgID, graph), Relation: RepoAdmin, SubjectType: TypeUser, SubjectID: userID, CreatedBy: p.User.ID, CreatedAt: tx.now()}
		if _, err := tx.St.DeleteTuples(ctx, TupleFilter{ObjectType: t.ObjectType, ObjectID: t.ObjectID, SubjectType: TypeUser, SubjectID: userID}); err != nil {
			return struct{}{}, err
		}
		if err := tx.St.WriteTuple(ctx, t); err != nil {
			return struct{}{}, err
		}
		e := actorEvent(p, "repo.admin.recover", orgID, graph+" "+userID)
		e.Detail = fmt.Sprintf("repaired %d malformed grants", repaired)
		tx.audit(ctx, e)
		return struct{}{}, nil
	})
	return err
}

func (s *Service) hasRepoAdmin(ctx context.Context, orgID, graph string) (bool, error) {
	ms, err := s.St.OrgMembers(ctx, orgID)
	if err != nil {
		return false, err
	}
	if len(ms) > MaxTuples {
		return false, ErrAccessLimit
	}
	for _, m := range ms {
		u, err := s.St.UserByID(ctx, m.User.ID)
		if err != nil {
			return false, err
		}
		if u.DisabledAt != nil {
			continue
		}
		a, err := s.accessOf(ctx, u.ID)
		// Root's grants never make it a working administrator.
		if errors.Is(err, ErrInvalidInput) || errors.Is(err, ErrAccessLimit) || errors.Is(err, ErrRootAccount) {
			continue
		}
		if err != nil {
			return false, err
		}
		for _, r := range a.Repos {
			if r.OrgID == orgID && r.Graph == graph && r.Role == Admin {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *Service) guardRepoAdmin(ctx context.Context, orgID, graph string) error {
	ok, err := s.hasRepoAdmin(ctx, orgID, graph)
	if err != nil {
		return err
	}
	if !ok {
		return ErrLastRepoAdmin
	}
	return nil
}

// AccessChange describes the permission migration without granting compatibility
// access to platform administrators or external shares.
type AccessChange struct {
	OrgID                  string   `json:"org_id"`
	DefaultRepoAccess      string   `json:"default_repo_access"`
	RepositoryCount        int      `json:"repository_count"`
	OrphanedRepositories   []string `json:"orphaned_repositories"`
	InactiveExternalGrants int      `json:"inactive_external_grants"`
	MalformedGrants        int      `json:"malformed_grants"`
	UnresolvedRepositories []string `json:"unresolved_repositories"`
}

// AccessChangeReport identifies entries requiring explicit owner recovery.
func (s *Service) AccessChangeReport(ctx context.Context, orgID string) (AccessChange, error) {
	report := AccessChange{OrgID: orgID, OrphanedRepositories: []string{}, UnresolvedRepositories: []string{}}
	o, err := s.St.OrgByID(ctx, orgID)
	if err != nil {
		return report, err
	}
	report.DefaultRepoAccess = o.DefaultRepoAccess
	repos, err := s.St.OrgRepositories(ctx, orgID)
	if err != nil {
		return report, err
	}
	report.RepositoryCount = len(repos)
	for _, r := range repos {
		if r.Usable() {
			ok, err := s.hasRepoAdmin(ctx, orgID, r.Graph)
			if errors.Is(err, ErrAccessLimit) || errors.Is(err, ErrInvalidInput) {
				report.UnresolvedRepositories = append(report.UnresolvedRepositories, r.Graph)
			} else if err != nil {
				return report, err
			}
			if !ok && err == nil {
				report.OrphanedRepositories = append(report.OrphanedRepositories, r.Graph)
			}
		}
		ts, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectID: RepoObject(orgID, r.Graph)})
		if errors.Is(err, ErrAccessLimit) {
			report.UnresolvedRepositories = append(report.UnresolvedRepositories, r.Graph)
			continue
		}
		if err != nil {
			return report, err
		}
		for _, t := range ts {
			if validRepoTuple(t) != nil {
				report.MalformedGrants++
			}
			g, err := s.describe(ctx, orgID, t)
			if errors.Is(err, ErrNotFound) || g.External {
				report.InactiveExternalGrants++
			} else if err != nil && !errors.Is(err, ErrInvalidInput) {
				return report, err
			}
		}
	}
	return report, nil
}

func (s *Service) adminRepositories(ctx context.Context, orgID string) ([]string, error) {
	repos, err := s.St.OrgRepositories(ctx, orgID)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, r := range repos {
		if !r.Usable() {
			continue
		}
		ok, err := s.hasRepoAdmin(ctx, orgID, r.Graph)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r.Graph)
		}
	}
	return out, nil
}

func (s *Service) preserveAdmins(ctx context.Context, orgID string, graphs []string) error {
	for _, graph := range graphs {
		if err := s.guardRepoAdmin(ctx, orgID, graph); err != nil {
			return err
		}
	}
	return nil
}

// repairRepoGrants preserves denials for other affected members when an owner
// replaces malformed legacy policy with an explicit administrator assignment.
func (s *Service) repairRepoGrants(ctx context.Context, p Principal, orgID, graph string) (int, error) {
	object := RepoObject(orgID, graph)
	grants, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectID: object})
	if err != nil {
		return 0, err
	}
	denied := map[string]bool{}
	repaired := 0
	for _, grant := range grants {
		if validRepoTuple(grant) == nil {
			continue
		}
		switch grant.SubjectType {
		case TypeUser:
			denied[grant.SubjectID] = true
		case TypeTeam:
			places, err := s.St.ReadTuples(ctx, TupleFilter{ObjectType: TypeTeam, ObjectID: grant.SubjectID, SubjectType: TypeUser})
			if err != nil {
				return 0, err
			}
			for _, place := range places {
				denied[place.SubjectID] = true
			}
		}
		if len(denied) > MaxTuples {
			return 0, ErrAccessLimit
		}
		if err := s.St.DeleteTuple(ctx, grant); err != nil {
			return 0, err
		}
		repaired++
	}
	for userID := range denied {
		if _, err := s.St.MemberRole(ctx, orgID, userID); errors.Is(err, ErrNotFound) {
			continue
		} else if err != nil {
			return 0, err
		}
		if _, err := s.St.DeleteTuples(ctx, TupleFilter{ObjectType: TypeRepository, ObjectID: object, SubjectType: TypeUser, SubjectID: userID}); err != nil {
			return 0, err
		}
		if err := s.St.WriteTuple(ctx, Tuple{ObjectType: TypeRepository, ObjectID: object, Relation: RepoNone, SubjectType: TypeUser, SubjectID: userID, CreatedBy: p.User.ID, CreatedAt: s.now()}); err != nil {
			return 0, err
		}
	}
	return repaired, nil
}
