package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func fastRetry(t *testing.T, attempts int) {
	t.Helper()
	oldA, oldB := remoteAttempts, remoteBackoff
	remoteAttempts, remoteBackoff = attempts, func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { remoteAttempts, remoteBackoff = oldA, oldB })
}

func TestRetryRemote(t *testing.T) {
	dns := errors.New("git clone: exit status 128: Could not resolve host: github.com (Timeout while contacting DNS servers)")
	cases := []struct {
		name  string
		errs  []error // one per call; nil succeeds
		calls int
		fails bool
	}{
		{"succeeds first time", []error{nil}, 1, false},
		{"recovers after DNS failures", []error{dns, dns, nil}, 3, false},
		{"gives up after the last attempt", []error{dns, dns, dns, dns, dns}, 4, true},
		{"no retry for refused access", []error{gitx.ErrNoAccess, nil}, 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastRetry(t, 4)
			calls := 0
			err := retryRemote(context.Background(), "clone", func(context.Context) error {
				calls++
				return tc.errs[calls-1]
			})
			assert.Equal(t, calls, tc.calls)
			assert.Equal(t, err != nil, tc.fails)
		})
	}
}

func TestRetryRemoteStopsWhenCancelled(t *testing.T) {
	oldB := remoteBackoff
	remoteBackoff = func(int) time.Duration { return time.Hour }
	t.Cleanup(func() { remoteBackoff = oldB })
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retryRemote(ctx, "clone", func(context.Context) error {
		calls++
		cancel()
		return errors.New("Could not resolve host")
	})
	require.Error(t, err)
	assert.Equal(t, calls, 1)
}
