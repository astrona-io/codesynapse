package security

import (
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
)

// ApprovedOptions applies a tenant's current approval to the indexed policy.
// A failed check preserves every finding and its grading penalty.
func ApprovedOptions(minSeverity, tenant, repo, digest string, approval accounts.RepoPolicyApproval) Options {
	opts := Options{MinSeverity: minSeverity}
	if tenant == "" || repo == "" || digest == "" || approval.OrgID != tenant || approval.Graph != repo || approval.Digest != digest || !approval.AllowSuppressions || (approval.ExpiresAt != nil && !approval.ExpiresAt.After(time.Now())) {
		return opts
	}
	policy, err := repoconfig.ParseCanonicalJSON([]byte(approval.ConfigJSON))
	if err != nil || policy.Digest() != digest {
		return opts
	}
	opts.Suppression = func(n model.Node) string {
		if n.Repo == repo && policy.Suppresses(n.Rule, n.File) {
			return approval.Reason
		}
		return ""
	}
	return opts
}
