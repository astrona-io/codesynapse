package mcpserver

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
)

type policyAccess interface {
	RepoPolicy(context.Context, string, string) (accounts.RepoPolicyApproval, error)
}

// RepoPolicy reads approval metadata; MCP has no approval mutation tool.
func (a AccountsAccess) RepoPolicy(ctx context.Context, org, repo string) (accounts.RepoPolicyApproval, error) {
	return a.Acc.St.GetRepoPolicy(ctx, org, repo)
}

func (h *handlers) securityOptions(ctx context.Context, repo, minSeverity string) security.Options {
	opts := security.Options{MinSeverity: minSeverity}
	caller := callerFrom(ctx)
	access, ok := h.access.(policyAccess)
	if caller == nil || !ok {
		return opts
	}
	node, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return opts
	}
	metadata, err := ingest.RepositoryPolicy(node)
	if err != nil {
		return opts
	}
	approval, err := access.RepoPolicy(ctx, caller.OrgID, repo)
	if err != nil {
		return opts
	}
	return security.ApprovedOptions(minSeverity, caller.OrgID, repo, metadata.Digest, approval)
}
