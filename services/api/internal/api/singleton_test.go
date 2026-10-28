package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func platformSample(t *testing.T, line string) float64 {
	t.Helper()
	var text bytes.Buffer
	require.NoError(t, telemetry.Default.WriteText(&text))
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(line) + ` (\S+)$`).FindStringSubmatch(text.String())
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	require.NoError(t, err)
	return v
}

// TestLoopsLeaseIsMeasured: taking the api-loops lease is counted and the
// process reports that it holds it, with no tenant on either series.
func TestLoopsLeaseIsMeasured(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	const acquired = `platform_codesynapse_lease_events_total{lease="api_loops",event="acquired"}`
	before := platformSample(t, acquired)
	started := make(chan struct{})
	s := &Server{cfg: Config{Accounts: accounts.NewService(acctmem.New(), true)}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.singleton(ctx, func(ctx context.Context) { close(started); <-ctx.Done() })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the only process never took the lease")
	}
	deadline := time.Now().Add(5 * time.Second)
	for platformSample(t, acquired) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.Equal(t, platformSample(t, acquired), before+1)
	assert.Equal(t, platformSample(t, "platform_codesynapse_loops_lease_held"), 1.0)
}
