package api

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/query"
)

// Projection failures delay explanations; PostgreSQL still decides access.
func (s *Server) projectAccess(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		run, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := query.ProjectAccessPolicies(run, s.cfg.Accounts.St, s.st, 100)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn("access projection deferred", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
