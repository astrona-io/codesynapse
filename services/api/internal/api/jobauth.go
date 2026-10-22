package api

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// jobAuthorization captures identity, never a request context or bearer token.
func (s *Server) jobAuthorization(ctx context.Context, repo string, least accounts.Role) (string, func(context.Context) error) {
	id, err := execution.Require(ctx)
	if err != nil {
		return "", func(context.Context) error { return err }
	}
	if s.cfg.Accounts == nil {
		return "", func(context.Context) error { return errExecutionRegistry }
	}
	p, ok := principal(ctx)
	sc := scopeFrom(ctx)
	reach, found := sc.reach[repo]
	if !found && len(sc.orgs) == 1 {
		reach.OrgID, found = sc.orgs[0].Org.ID, true
	}
	if !ok || !found || reach.Ambiguous || reach.OrgID == "" || id.SubjectID != p.User.ID || id.TenantID != reach.OrgID {
		return "", func(context.Context) error { return accounts.ErrForbidden }
	}
	org := reach.OrgID
	return org, func(ctx context.Context) error {
		current, err := execution.Require(ctx)
		if err != nil || current != id {
			return execution.ErrConflict
		}
		service := s.accountsFor(ctx)
		_, err = service.ResolveExecutionSession(ctx, p.User.ID, org, id.SessionID)
		if err == nil {
			_, err = service.ResolveRepository(ctx, p, org, repo, least)
		}
		if err != nil {
			s.recordJobDenial(ctx, accounts.Event{UserID: p.User.ID, Via: p.Via, KeyID: p.KeyID,
				OrgID: org, Action: "job.authorization_denied", Target: repo, Outcome: accounts.Failure})
		}
		return err
	}
}

func checkJob(ctx context.Context, check func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := execution.Require(ctx); err != nil {
		return err
	}
	if check != nil {
		return check(ctx)
	}
	return nil
}

func identityOf(ctx context.Context) execution.Identity {
	id, _ := execution.From(ctx)
	return id
}
