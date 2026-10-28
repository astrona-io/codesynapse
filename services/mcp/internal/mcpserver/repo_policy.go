package mcpserver

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
)

type policyAccess interface {
	RepoPolicy(context.Context, string, string) (accounts.RepoPolicyApproval, error)
	FindingTriage(context.Context, string, string) ([]accounts.FindingTriage, error)
}

// RepoPolicy reads approval metadata; MCP has no approval mutation tool.
func (a AccountsAccess) RepoPolicy(ctx context.Context, org, repo string) (accounts.RepoPolicyApproval, error) {
	return a.Acc.St.GetRepoPolicy(ctx, org, repo)
}

// FindingTriage reads the organisation's triage decisions; they are made
// in the dashboard or the API, never through MCP.
func (a AccountsAccess) FindingTriage(ctx context.Context, org, repo string) ([]accounts.FindingTriage, error) {
	return a.Acc.St.ListFindingTriage(ctx, org, repo, accounts.MaxFindingTriage)
}

// securityOptions applies the caller organisation's approved suppressions
// and triage decisions; whatever cannot be read leaves findings active.
func (h *handlers) securityOptions(ctx context.Context, repo, minSeverity string) security.Options {
	opts := security.Options{MinSeverity: minSeverity}
	caller := callerFrom(ctx)
	access, ok := h.access.(policyAccess)
	if caller == nil || !ok {
		return opts
	}
	opts = h.approvedOptions(ctx, access, caller.OrgID, repo, opts)
	if rows, err := access.FindingTriage(ctx, caller.OrgID, repo); err == nil {
		opts = security.WithTriage(opts, rows, time.Now())
	}
	return opts
}

func (h *handlers) approvedOptions(ctx context.Context, access policyAccess, org, repo string, opts security.Options) security.Options {
	node, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return opts
	}
	metadata, err := ingest.RepositoryPolicy(node)
	if err != nil {
		return opts
	}
	approval, err := access.RepoPolicy(ctx, org, repo)
	if err != nil {
		return opts
	}
	return security.ApprovedOptions(opts.MinSeverity, org, repo, metadata.Digest, approval)
}
