package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingestreport"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestIngestionReportTenantScope(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	p, err := acc.AuthenticateKey(t.Context(), keys["a"])
	require.NoError(t, err)
	id, err := acc.ResolveExecutionSession(t.Context(), p.User.ID, p.OrgID, "chat-a")
	require.NoError(t, err)
	ctx, err := execution.With(t.Context(), id)
	require.NoError(t, err)
	ledger, err := ingestreport.New("mcp-report-run", time.Now().UTC())
	require.NoError(t, err)
	run := accounts.IngestRun{OrgID: p.OrgID, Graph: storetest.Repo, Identity: id, Report: ledger.Snapshot()}
	require.NoError(t, acc.St.BeginIngestRun(ctx, run))
	run.Report = ledger.Finish(ingestreport.Failed, ingestreport.CommitNone)
	require.NoError(t, acc.St.SaveIngestRun(ctx, run))
	a, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	b, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	c, err := connectKey(t, url, keys["c"], "chat-c")
	require.NoError(t, err)
	var own, other ingestionReportOut
	require.False(t, call(t, a, "ingestion_report", map[string]any{"repo": storetest.Repo}, &own).IsError)
	require.Len(t, own.History, 1)
	raw, err := json.Marshal(own)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "chat-a"))
	require.False(t, call(t, b, "ingestion_report", map[string]any{"repo": storetest.Repo}, &other).IsError)
	assert.Len(t, other.History, 0)
	assert.True(t, call(t, c, "ingestion_report", map[string]any{"repo": storetest.Repo}, nil).IsError)
}
