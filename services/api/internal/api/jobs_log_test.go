package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// A worker's log must show how every job ended, not only the ones that failed.
func TestJobLogsHowItEnded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{"succeeded", nil, "job finished", "job failed"},
		{"failed", errors.New("boom"), "job failed", "job finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out bytes.Buffer
			jobs := NewJobs(ctx, memory.New(), slog.New(slog.NewTextHandler(&out, nil)), 0, false, false)
			id := fixedExecution()
			done := make(chan error, 1)
			_, err := jobs.Start("repo", "", JobOptions{Kind: "ai", Task: "security-triage", Identity: id, OrgID: id.TenantID,
				Run:  func(context.Context) (string, error) { return "review", tc.err },
				Done: func(err error) { done <- err }})
			require.NoError(t, err)
			select {
			case got := <-done:
				assert.Equal(t, got != nil, tc.err != nil)
			case <-time.After(5 * time.Second):
				t.Fatal("job did not finish")
			}
			log := out.String()
			assert.Contains(t, log, tc.want)
			assert.Contains(t, log, "kind=ai")
			if bytes.Contains(out.Bytes(), []byte(tc.notWant)) {
				t.Errorf("log has %q for a %s job:\n%s", tc.notWant, tc.name, log)
			}
		})
	}
}
