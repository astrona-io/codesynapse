package accounts

import (
	"context"
	"fmt"
)

// Repositories live in one of two spaces of an organisation. Organisation
// repositories are added by owners and admins and follow the organisation's
// default access and grants. A member may also add a repository to their
// own space while the organisation allows it (the default): only they, and
// whom they grant, can use it. A repository has one entry per organisation,
// so the same one cannot sit in both spaces.

// MemberReposSetting says whether members of the organisation may add
// repositories to their own space.
func (s *Service) MemberReposSetting(ctx context.Context, p Principal, orgID string) (bool, error) {
	if err := s.Require(ctx, p, orgID, Member); err != nil {
		return false, err
	}
	return s.St.MemberReposEnabled(ctx, orgID)
}

// SetMemberRepos allows or stops members adding repositories to their own
// space. Entries already added stay. Only owners and admins in a signed-in
// session may change it; the change is audited first, and a failed audit
// refuses it.
func (s *Service) SetMemberRepos(ctx context.Context, p Principal, orgID string, enabled bool) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	e := actorEvent(p, "org.member_repos.set", orgID, orgID)
	e.Detail = fmt.Sprintf("enabled=%t", enabled)
	if _, err := s.Record(ctx, e); err != nil {
		return err
	}
	return s.St.SetMemberReposEnabled(ctx, orgID, enabled)
}
