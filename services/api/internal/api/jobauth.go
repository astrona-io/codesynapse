package api

import (
	"context"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
)

// jobAuthorization captures identity, never a request context or bearer token.
func (s *Server) jobAuthorization(ctx context.Context, repo string, least accounts.Role) (string, func(context.Context) error) {
	if s.cfg.Accounts == nil {
		return "", nil
	}
	p, ok := principal(ctx)
	sc := scopeFrom(ctx)
	reach, found := sc.reach[repo]
	if !found && len(sc.orgs) == 1 {
		reach.OrgID, found = sc.orgs[0].Org.ID, true
	}
	if !ok || !found || reach.Ambiguous || reach.OrgID == "" {
		return "", func(context.Context) error { return accounts.ErrForbidden }
	}
	org := reach.OrgID
	return org, func(ctx context.Context) error {
		_, err := s.cfg.Accounts.ResolveRepository(ctx, p, org, repo, least)
		if err != nil {
			_, auditErr := s.cfg.Accounts.Record(ctx, accounts.Event{UserID: p.User.ID, Via: p.Via, KeyID: p.KeyID,
				OrgID: org, Action: "job.authorization_denied", Target: repo, Outcome: accounts.Failure})
			if auditErr != nil && s.logger != nil {
				s.logger.Warn("job authorization audit failed", "error", auditErr)
			}
		}
		return err
	}
}

func checkJob(ctx context.Context, check func(context.Context) error) error {
	if check != nil {
		return check(ctx)
	}
	return nil
}
