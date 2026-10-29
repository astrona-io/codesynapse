package gitx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestRemoteOutcome(t *testing.T) {
	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	gone, stop := context.WithCancel(context.Background())
	stop()
	exit := errors.New("exit status 128")
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"ok", context.Background(), nil, telemetry.GitOK},
		{"refused", context.Background(), fmt.Errorf("git clone: %w: fatal", ErrNoAccess), telemetry.GitNoAccess},
		{"moved", context.Background(), fmt.Errorf("git fetch: %w", ErrRedirected), telemetry.GitRedirected},
		{"deadline", expired, exit, telemetry.GitTimeout},
		{"cancelled is not counted", gone, exit, ""},
		// The message is never read: text that looks like a refusal stays an error.
		{"other", context.Background(), errors.New("git push: authentication failed"), telemetry.GitError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, remoteOutcome(c.ctx, c.err), c.want)
		})
	}
	for _, sub := range []string{"clone", "fetch", "ls-remote", "push"} {
		assert.True(t, remoteOps[sub] != "", sub)
	}
}
