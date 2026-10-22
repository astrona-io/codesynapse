package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ingestReportAccess interface {
	IngestReports(context.Context, string, string) ([]accounts.IngestRun, *accounts.IngestRun, error)
}

// IngestReports reads only the resolved tenant's bounded report summaries.
func (a AccountsAccess) IngestReports(ctx context.Context, org, repo string) ([]accounts.IngestRun, *accounts.IngestRun, error) {
	runs, err := a.Acc.St.ListIngestRuns(ctx, org, repo, 5)
	if err != nil {
		return nil, nil, err
	}
	for i := range runs {
		runs[i].Report.Files = nil
	}
	success, err := a.Acc.St.LatestSuccessfulIngestRun(ctx, org, repo)
	if errors.Is(err, accounts.ErrNotFound) {
		return runs, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	success.Report.Files = nil
	return runs, &success, nil
}

type ingestionReportOut struct {
	History        []accounts.IngestRun `json:"history"`
	LastSuccessful *accounts.IngestRun  `json:"last_successful"`
	Notice         string               `json:"notice"`
}

func (h *handlers) ingestionReport(ctx context.Context, _ *mcp.CallToolRequest, in RepoArg) (*mcp.CallToolResult, ingestionReportOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, ingestionReportOut{}, err
	}
	caller := callerFrom(ctx)
	access, ok := h.access.(ingestReportAccess)
	if caller == nil || !ok {
		return nil, ingestionReportOut{}, errors.New("tenant ingestion reports require accounts")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	runs, success, err := access.IngestReports(ctx, caller.OrgID, repo)
	if runs == nil {
		runs = []accounts.IngestRun{}
	}
	return nil, ingestionReportOut{History: runs, LastSuccessful: success,
		Notice: "Counts describe encountered files in each run; pruned descendants are unknown. Context estimates are metadata-based, not billed tokens. Annotation outcomes are separate."}, err
}
