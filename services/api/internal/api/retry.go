package api

import (
	"context"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/logx"
)

// remoteAttempts is how many times a clone or fetch is tried before the job
// fails; remoteBackoff is the wait before each retry (a variable so tests
// need not sleep).
var (
	remoteAttempts = 4
	remoteBackoff  = func(attempt int) time.Duration { return time.Duration(attempt*attempt) * 5 * time.Second }
)

// retryRemote runs fn, trying again after a pause while it fails with a
// network or host error (DNS, TLS, timeouts, 5xx). Every failed attempt is
// logged with its error, so an outage shows in the worker log before the job
// gives up; the last error is returned unchanged for the job record.
func retryRemote(ctx context.Context, what string, fn func(context.Context) error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(ctx); err == nil || !gitx.IsTransient(err) {
			return err
		}
		if attempt >= remoteAttempts {
			logx.FromContext(ctx).Error("remote unreachable, giving up", "what", what, "attempts", attempt, logx.Error(err))
			return fmt.Errorf("%w (gave up after %d attempts)", err, attempt)
		}
		wait := remoteBackoff(attempt)
		logx.FromContext(ctx).Warn("remote unreachable, retrying", "what", what, "attempt", attempt, "retry_in", wait.String(), logx.Error(err))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return err
		}
	}
}
