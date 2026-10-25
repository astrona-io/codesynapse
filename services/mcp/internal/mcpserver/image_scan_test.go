package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/imagescan"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The security report counts the caller's own organisation's image scan,
// never another's.
func TestSecurityReportCountsImageScan(t *testing.T) {
	url, keys, acc, _, reader := keyServer(t)
	ctx := context.Background()
	require.NoError(t, reader.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	a, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	require.NoError(t, acc.St.SetImageScan(ctx, agentmem.Namespace(a.OrgID, storetest.Repo), accounts.ImageScan{
		UploadedAt: time.Now(),
		Findings:   []imagescan.Finding{{Image: "app:1", Package: "zlib", Version: "1.2", ID: "CVE-2025-0002", Severity: "critical"}},
	}))
	ca, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	cb, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	var mine, theirs security.Report
	require.False(t, call(t, ca, "security_report", map[string]any{}, &mine).IsError)
	require.False(t, call(t, cb, "security_report", map[string]any{}, &theirs).IsError)
	require.NotNil(t, mine.ContainerImages)
	assert.Equal(t, mine.ContainerImages.Counts["critical"], 1)
	assert.Equal(t, mine.ContainerImages.Images[0], "app:1")
	assert.True(t, theirs.ContainerImages == nil, "another organisation sees no scan")
}
