package accounts

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/redact"
)

// Finding triage: an organisation's decision about one security finding of
// one of its repositories (accepted risk, false positive, fixed by a commit
// or pull request), with who made it, when and why. It lives here, per
// organisation and repository entry, and never in the graph, which
// organisations share: the finding itself stays stored and only this
// organisation's report treats it as suppressed.

// Triage states.
const (
	TriageAcceptedRisk  = "accepted_risk"
	TriageFalsePositive = "false_positive"
	TriageFixedBy       = "fixed_by"
)

// TriageStates lists the valid states.
var TriageStates = []string{TriageAcceptedRisk, TriageFalsePositive, TriageFixedBy}

// Limits on triage records.
const (
	// MaxFindingTriage bounds the decisions kept per repository entry.
	MaxFindingTriage = 5000
	// MaxTriageReason bounds the reason in bytes, after redaction.
	MaxTriageReason = 1000
)

var (
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitRefPattern   = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	prRefPattern       = regexp.MustCompile(`^#?[1-9][0-9]{0,8}$`)
)

// FindingTriage is one organisation's triage decision about one finding.
type FindingTriage struct {
	OrgID string `json:"org_id"`
	Graph string `json:"graph"`
	// Fingerprint is model.FindingFingerprint of the finding: rule, owning
	// symbol and file, so the decision survives line moves.
	Fingerprint string `json:"fingerprint"`
	// Rule and File describe the finding when it was triaged, so the
	// decision still reads after the finding is gone.
	Rule  string `json:"rule"`
	File  string `json:"file"`
	State string `json:"state"`
	// Reference is the commit SHA or pull request number ("#123") of a
	// fixed_by decision; empty otherwise.
	Reference string `json:"reference,omitempty"`
	Reason    string `json:"reason"`
	ActorID   string `json:"actor_id"`
	// ActorName is filled on read from the actor's account.
	ActorName string     `json:"actor_name,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Active reports whether the decision applies at now.
func (t FindingTriage) Active(now time.Time) bool {
	return t.ExpiresAt == nil || t.ExpiresAt.After(now)
}

// NormalizeTriageReference validates the reference of a fixed_by decision:
// a commit SHA (7 to 64 hex characters) or a pull request number, which is
// returned as "#123".
func NormalizeTriageReference(ref string) (string, bool) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	switch {
	case commitRefPattern.MatchString(ref) && !prRefPattern.MatchString(ref):
		return ref, true
	case prRefPattern.MatchString(ref):
		return "#" + strings.TrimPrefix(ref, "#"), true
	}
	return "", false
}

// PrepareFindingTriage validates t and redacts its reason. Stores call it
// again so nothing unvalidated is written.
func PrepareFindingTriage(t FindingTriage) (FindingTriage, error) {
	if !policyText(t.OrgID, 128) || !policyText(t.Graph, 512) || !policyText(t.ActorID, 128) || !fingerprintPattern.MatchString(t.Fingerprint) ||
		!policyText(t.Rule, 128) || !policyText(t.File, 1024) || t.CreatedAt.IsZero() || (t.ExpiresAt != nil && !t.ExpiresAt.After(t.CreatedAt)) {
		return FindingTriage{}, ErrInvalidInput
	}
	switch t.State {
	case TriageAcceptedRisk, TriageFalsePositive:
		if t.Reference != "" {
			return FindingTriage{}, ErrInvalidInput
		}
	case TriageFixedBy:
		ref, ok := NormalizeTriageReference(t.Reference)
		if !ok {
			return FindingTriage{}, ErrInvalidInput
		}
		t.Reference = ref
	default:
		return FindingTriage{}, ErrInvalidInput
	}
	// One line: the reason is shown in tables and Markdown.
	t.Reason = redact.Free(strings.Join(strings.Fields(t.Reason), " "))
	if !policyText(t.Reason, MaxTriageReason) {
		return FindingTriage{}, ErrInvalidInput
	}
	t.ActorName = ""
	if t.ExpiresAt != nil {
		at := *t.ExpiresAt
		t.ExpiresAt = &at
	}
	return t, nil
}

// SetFindingTriage records or replaces the caller's organisation's decision
// about a finding. It needs admin access to the repository, from a
// signed-in session or an API key; the record and its audit event commit
// together.
func (s *Service) SetFindingTriage(ctx context.Context, p Principal, t FindingTriage) (FindingTriage, error) {
	return policyChange(ctx, s, func(tx *Service) (FindingTriage, error) {
		if p.Via != "session" && p.Via != "key" {
			return FindingTriage{}, ErrForbidden
		}
		if _, err := tx.ResolveRepository(ctx, p, t.OrgID, t.Graph, Admin); err != nil {
			return FindingTriage{}, err
		}
		t.ActorID, t.CreatedAt = p.User.ID, tx.now()
		prepared, err := PrepareFindingTriage(t)
		if err != nil {
			return FindingTriage{}, err
		}
		if err := tx.St.PutFindingTriage(ctx, prepared); err != nil {
			return FindingTriage{}, err
		}
		e := actorEvent(p, "finding.triage.set", t.OrgID, t.Graph)
		e.Detail = strings.Join(strings.Fields(prepared.State+" "+prepared.Reference+" "+prepared.Rule+" "+prepared.Fingerprint[:16]+" "+prepared.Reason), " ")
		tx.audit(ctx, e)
		prepared.ActorName = p.User.Name
		return prepared, nil
	})
}

// ClearFindingTriage removes the organisation's decision, so the finding
// counts again. The audit log keeps what it was.
func (s *Service) ClearFindingTriage(ctx context.Context, p Principal, orgID, graph, fingerprint string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if p.Via != "session" && p.Via != "key" {
			return struct{}{}, ErrForbidden
		}
		if !fingerprintPattern.MatchString(fingerprint) {
			return struct{}{}, ErrInvalidInput
		}
		if _, err := tx.ResolveRepository(ctx, p, orgID, graph, Admin); err != nil {
			return struct{}{}, err
		}
		if err := tx.St.DeleteFindingTriage(ctx, orgID, graph, fingerprint); err != nil {
			return struct{}{}, err
		}
		e := actorEvent(p, "finding.triage.clear", orgID, graph)
		e.Detail = fingerprint[:16]
		tx.audit(ctx, e)
		return struct{}{}, nil
	})
	return err
}
