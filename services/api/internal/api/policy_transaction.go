package api

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
)

type accountTransactionKey struct{}
type accountTransaction struct {
	server      *Server
	service     *accounts.Service
	denied      []accounts.Event
	afterCommit []func()
}

// accountsFor uses the already locked policy snapshot during graph publication.
// Returning to the original store inside its transaction would deadlock memory
// adapters and could observe inconsistent policy on PostgreSQL.
func (s *Server) accountsFor(ctx context.Context) *accounts.Service {
	if tx, ok := ctx.Value(accountTransactionKey{}).(*accountTransaction); ok && tx.server == s {
		return tx.service
	}
	return s.cfg.Accounts
}

func (s *Server) publishRepo(ctx context.Context, graph string, publish func(context.Context) error) error {
	if s.cfg.Accounts == nil {
		return publish(ctx)
	}
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	var txContext *accountTransaction
	err = s.cfg.Accounts.St.PolicyTransaction(ctx, id.TenantID, func(st accounts.Store) error {
		service := *s.cfg.Accounts
		service.St, service.Sink = st, nil
		txContext = &accountTransaction{server: s, service: &service}
		workCtx := context.WithValue(ctx, accountTransactionKey{}, txContext)
		if _, err := service.ResolveExecutionSession(workCtx, id.SubjectID, id.TenantID, id.SessionID); err != nil {
			return err
		}
		if err := s.reconcileIngestReports(workCtx, graph); err != nil {
			return err
		}
		return publish(workCtx)
	})
	// Denials must survive the rollback that kept the previous graph active.
	if txContext != nil {
		if err == nil {
			for _, committed := range txContext.afterCommit {
				committed()
			}
		}
		for _, event := range txContext.denied {
			if _, auditErr := s.cfg.Accounts.Record(ctx, event); auditErr != nil && s.logger != nil {
				s.logger.Warn("job authorization audit failed", "error", auditErr)
			}
		}
	}
	return err
}

func (s *Server) recordJobDenial(ctx context.Context, event accounts.Event) {
	if tx, ok := ctx.Value(accountTransactionKey{}).(*accountTransaction); ok && tx.server == s {
		if len(tx.denied) < 4 {
			tx.denied = append(tx.denied, event)
		}
		return
	}
	if _, err := s.accountsFor(ctx).Record(ctx, event); err != nil && s.logger != nil {
		s.logger.Warn("job authorization audit failed", "error", err)
	}
}
