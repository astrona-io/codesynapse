package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// ProjectAccessPolicies retries the durable dirty-org outbox. A failed build
// stays pending; acknowledgement cannot clear a newer committed revision.
func ProjectAccessPolicies(ctx context.Context, source accounts.Store, target store.AccessStore, limit int) error {
	orgs, err := source.PendingAccessPolicies(ctx, store.ClampLimit(limit, 20))
	if err != nil {
		return err
	}
	var failures []error
	for _, org := range orgs {
		snapshot, err := AccessSnapshot(ctx, source, org.ID)
		if err == nil {
			err = target.ReplaceAccess(ctx, snapshot)
		}
		if err == nil {
			err = source.AcknowledgeAccessPolicy(ctx, org.ID, snapshot.Revision)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("project organisation %s: %w", org.ID, err))
		}
	}
	return errors.Join(failures...)
}

// AccessSnapshot reads one stable revision. Concurrent policy changes abort the
// build, so retrying the outbox cannot publish a mixture of different revisions.
func AccessSnapshot(ctx context.Context, source accounts.Store, orgID string) (model.AccessSnapshot, error) {
	o, err := source.OrgByID(ctx, orgID)
	if err != nil {
		return model.AccessSnapshot{}, err
	}
	s := model.AccessSnapshot{TenantID: orgID, Revision: o.PolicyRevision, Default: o.DefaultRepoAccess}
	members, err := source.OrgMembers(ctx, orgID)
	if err != nil {
		return s, err
	}
	if len(members) > model.MaxAccessItems {
		return s, accounts.ErrAccessLimit
	}
	users := map[string]bool{}
	for _, m := range members {
		s.Users = append(s.Users, model.AccessUser{ID: m.User.ID, Active: m.User.DisabledAt == nil, InheritsDefault: m.Role != accounts.Guest})
		users[m.User.ID] = true
	}
	teams, err := source.OrgTeams(ctx, orgID)
	if err != nil {
		return s, err
	}
	repos, err := source.OrgRepositories(ctx, orgID)
	if err != nil {
		return s, err
	}
	if len(members)+len(teams)+len(repos) > model.MaxAccessItems {
		return s, accounts.ErrAccessLimit
	}
	teamIDs := map[string]bool{}
	for _, t := range teams {
		s.Teams = append(s.Teams, t.ID)
		teamIDs[t.ID] = true
	}
	var teamTuples []accounts.Tuple
	if len(s.Teams) > 0 {
		teamTuples, err = source.ReadTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeTeam, ObjectIDs: s.Teams})
		if err != nil {
			return s, err
		}
	}
	if len(members)+len(teams)+len(repos)+len(teamTuples) > model.MaxAccessItems {
		return s, accounts.ErrAccessLimit
	}
	for _, x := range teamTuples {
		if x.SubjectType != accounts.TypeUser || x.SubjectRelation != "" || !teamIDs[x.ObjectID] || (x.Relation != accounts.RelMember && x.Relation != accounts.RelMaintainer) {
			return s, accounts.ErrInvalidInput
		}
		if users[x.SubjectID] {
			s.Members = append(s.Members, model.AccessMembership{UserID: x.SubjectID, TeamID: x.ObjectID})
		}
	}
	objects := []string{}
	repoIDs := map[string]string{}
	for _, r := range repos {
		s.Repos = append(s.Repos, model.AccessRepo{ID: r.Graph, GraphID: r.Graph, Usable: r.Usable()})
		object := accounts.RepoObject(orgID, r.Graph)
		objects = append(objects, object)
		repoIDs[object] = r.Graph
	}
	var grants []accounts.Tuple
	if len(objects) > 0 {
		grants, err = source.ReadTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeRepository, ObjectIDs: objects})
		if err != nil {
			return s, err
		}
	}
	if len(members)+len(teams)+len(repos)+len(teamTuples)+len(grants) > model.MaxAccessItems {
		return s, accounts.ErrAccessLimit
	}
	for _, x := range grants {
		g := model.AccessGrant{SubjectID: x.SubjectID, RepoID: repoIDs[x.ObjectID]}
		if g.RepoID == "" {
			return s, accounts.ErrInvalidInput
		}
		switch x.SubjectType {
		case accounts.TypeUser:
			if x.SubjectRelation != "" {
				return s, accounts.ErrInvalidInput
			}
			if !users[x.SubjectID] {
				continue
			}
			g.SubjectKind = model.KindUser
		case accounts.TypeTeam:
			if !teamIDs[x.SubjectID] {
				continue
			}
			if x.SubjectRelation != accounts.RelMember {
				return s, accounts.ErrInvalidInput
			}
			g.SubjectKind = model.KindTeam
		default:
			return s, accounts.ErrInvalidInput
		}
		switch x.Relation {
		case accounts.RepoViewer:
			g.Role = "READ"
		case accounts.RepoEditor:
			g.Role = "WRITE"
		case accounts.RepoAdmin:
			g.Role = "ADMIN"
		case accounts.RepoNone:
			g.Role = "NONE"
		default:
			return s, accounts.ErrInvalidInput
		}
		s.Grants = append(s.Grants, g)
	}
	latest, err := source.OrgByID(ctx, orgID)
	if err != nil {
		return s, err
	}
	if latest.PolicyRevision != s.Revision {
		return s, errors.New("access policy changed during projection")
	}
	return s, s.Validate()
}
