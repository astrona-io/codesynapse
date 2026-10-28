package security

import (
	"context"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Triage is an organisation's recorded decision about a finding (see
// accounts.FindingTriage): what, who, when and why.
type Triage struct {
	State string `json:"state"`
	// Reference is the commit or pull request of a fixed_by decision.
	Reference string     `json:"reference,omitempty"`
	Reason    string     `json:"reason"`
	By        string     `json:"by,omitempty"`
	At        time.Time  `json:"at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

var triageLabels = map[string]string{
	accounts.TriageAcceptedRisk:  "Accepted risk",
	accounts.TriageFalsePositive: "False positive",
	accounts.TriageFixedBy:       "Fixed by",
}

// Label says the decision in one line, as the report's suppression reason.
func (t Triage) Label() string {
	label := triageLabels[t.State]
	if label == "" {
		label = t.State
	}
	if t.Reference != "" {
		label += " " + t.Reference
	}
	return label + ": " + t.Reason
}

// WithTriage adds the decisions in rows that are active at now to opts,
// keyed by fingerprint. Expired decisions are left out, so their findings
// count again.
func WithTriage(opts Options, rows []accounts.FindingTriage, now time.Time) Options {
	for _, r := range rows {
		if !r.Active(now) {
			continue
		}
		if opts.Triage == nil {
			opts.Triage = map[string]Triage{}
		}
		opts.Triage[r.Fingerprint] = Triage{State: r.State, Reference: r.Reference, Reason: r.Reason, By: r.ActorName, At: r.CreatedAt, ExpiresAt: r.ExpiresAt}
	}
	return opts
}

// FindingKey reads finding n's owner from r and returns its fingerprint,
// the key the report gives n.
func FindingKey(ctx context.Context, r store.Reader, n model.Node) (string, error) {
	owners, err := r.Traverse(ctx, n.ID, []model.EdgeKind{model.EdgeHasFinding}, store.In, 1, 1)
	if err != nil {
		return "", err
	}
	var owner *model.Node
	if len(owners) > 0 {
		owner = &owners[0].Node
	}
	return model.FindingFingerprint(n.Rule, model.FindingOwnerName(owner, n.File), n.File), nil
}
