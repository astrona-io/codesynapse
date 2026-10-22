package api

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/ingestreport"
)

// Check the original credential under the publication lock, including at queue
// insertion and checkpoints. Failure accounting uses the already bound run and
// remains possible after revocation; it cannot authorize a graph write.
func (j *Jobs) writeReport(ctx context.Context, job Job, report ingestreport.Report, authorize func(context.Context) error) error {
	write := func(workCtx context.Context) error {
		if err := checkJob(workCtx, authorize); err != nil {
			return err
		}
		return j.Report(workCtx, job, report)
	}
	_, locked := ctx.Value(accountTransactionKey{}).(*accountTransaction)
	if locked || j.Publish == nil {
		return write(ctx)
	}
	return j.Publish(ctx, job.Repo, write)
}
