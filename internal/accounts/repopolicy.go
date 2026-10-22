package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// MaxRepositoryAnnotations bounds tenant-owned metadata per repository.
const MaxRepositoryAnnotations = 5000

// RepoPolicyApproval approves precisely one normalized configuration digest.
type RepoPolicyApproval struct {
	OrgID             string     `json:"org_id"`
	Graph             string     `json:"graph"`
	Digest            string     `json:"digest"`
	ConfigJSON        string     `json:"config_json"`
	Reason            string     `json:"reason"`
	ActorID           string     `json:"actor_id"`
	AllowExclusions   bool       `json:"allow_exclusions"`
	AllowSuppressions bool       `json:"allow_suppressions"`
	AllowAnnotation   bool       `json:"allow_annotation"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

// RepositoryAnnotation is durable repository knowledge with original session
// provenance. It never modifies shared code symbols or enters chat memory.
type RepositoryAnnotation struct {
	OrgID           string    `json:"org_id"`
	Graph           string    `json:"graph"`
	SymbolID        string    `json:"symbol_id"`
	Commit          string    `json:"commit"`
	Revision        string    `json:"revision"`
	Fingerprint     string    `json:"fingerprint"`
	PolicyDigest    string    `json:"policy_digest"`
	JobID           string    `json:"job_id"`
	OriginSessionID string    `json:"origin_session_id"`
	Summary         string    `json:"summary"`
	Taxonomy        []string  `json:"taxonomy"`
	Confidence      *float64  `json:"confidence,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// PrepareRepoPolicy validates the serialized policy without trusting its caller
// to provide matching bytes and digest. API callers validate the config schema.
func PrepareRepoPolicy(p RepoPolicyApproval) (RepoPolicyApproval, error) {
	if !policyText(p.OrgID, 128) || !policyText(p.Graph, 512) || !policyText(p.ActorID, 128) || !policyText(p.Reason, 2048) || p.UpdatedAt.IsZero() || len(p.ConfigJSON) > 64<<10 || !json.Valid([]byte(p.ConfigJSON)) || (p.ExpiresAt != nil && !p.ExpiresAt.After(p.UpdatedAt)) {
		return RepoPolicyApproval{}, ErrInvalidInput
	}
	hash := sha256.Sum256([]byte(p.ConfigJSON))
	if p.Digest != hex.EncodeToString(hash[:]) {
		return RepoPolicyApproval{}, ErrInvalidInput
	}
	p.Reason = redact.Free(p.Reason)
	if !policyText(p.Reason, 2048) {
		return RepoPolicyApproval{}, ErrInvalidInput
	}
	if p.ExpiresAt != nil {
		at := *p.ExpiresAt
		p.ExpiresAt = &at
	}
	return p, nil
}

// SetRepoPolicy requires a current repository administrator's signed-in session.
// The approval and its semantic audit event commit in the same transaction.
func (s *Service) SetRepoPolicy(ctx context.Context, p Principal, approval RepoPolicyApproval) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if p.Via != "session" {
			return struct{}{}, ErrForbidden
		}
		if _, err := tx.ResolveRepository(ctx, p, approval.OrgID, approval.Graph, Admin); err != nil {
			return struct{}{}, err
		}
		approval.ActorID, approval.UpdatedAt = p.User.ID, tx.now()
		prepared, err := PrepareRepoPolicy(approval)
		if err != nil {
			return struct{}{}, err
		}
		if err := tx.St.PutRepoPolicy(ctx, prepared); err != nil {
			return struct{}{}, err
		}
		e := actorEvent(p, "repo.config.approve", approval.OrgID, approval.Graph)
		e.Detail = prepared.Digest + " " + prepared.Reason
		tx.audit(ctx, e)
		return struct{}{}, nil
	})
	return err
}

// DeleteRepoPolicy revokes the tenant's approval without changing shared data.
func (s *Service) DeleteRepoPolicy(ctx context.Context, p Principal, orgID, graph string) error {
	_, err := policyChange(ctx, s, func(tx *Service) (struct{}, error) {
		if p.Via != "session" {
			return struct{}{}, ErrForbidden
		}
		if _, err := tx.ResolveRepository(ctx, p, orgID, graph, Admin); err != nil {
			return struct{}{}, err
		}
		if err := tx.St.DeleteRepoPolicy(ctx, orgID, graph); err != nil {
			return struct{}{}, err
		}
		tx.audit(ctx, actorEvent(p, "repo.config.revoke", orgID, graph))
		return struct{}{}, nil
	})
	return err
}

// PrepareAnnotations enforces one scope per atomic batch and copies/redacts
// mutable fields. The caller still must check live authority and ingest revision.
func PrepareAnnotations(ctx context.Context, records []RepositoryAnnotation) ([]RepositoryAnnotation, error) {
	id, err := execution.Require(ctx)
	if err != nil {
		return nil, err
	}
	if len(records) < 1 || len(records) > 100 {
		return nil, ErrInvalidInput
	}
	out := slices.Clone(records)
	first := out[0]
	seen := make(map[string]bool, len(out))
	for i, r := range out {
		if r.OrgID != id.TenantID || r.OriginSessionID != id.SessionID || r.Graph != first.Graph || r.Revision != first.Revision || r.Commit != first.Commit || r.PolicyDigest != first.PolicyDigest || r.JobID != first.JobID || seen[r.SymbolID] || r.UpdatedAt.IsZero() {
			return nil, ErrInvalidInput
		}
		for _, field := range []string{r.Graph, r.SymbolID, r.Commit, r.Revision, r.Fingerprint, r.PolicyDigest, r.JobID} {
			if !policyText(field, 512) {
				return nil, ErrInvalidInput
			}
		}
		if !policyText(r.Summary, 1024) || len(r.Taxonomy) > 8 {
			return nil, ErrInvalidInput
		}
		seen[r.SymbolID] = true
		r.Summary, r.Taxonomy = redact.Free(r.Summary), slices.Clone(r.Taxonomy)
		if !policyText(r.Summary, 1024) {
			return nil, ErrInvalidInput
		}
		for j, taxonomy := range r.Taxonomy {
			if !policyText(taxonomy, 64) {
				return nil, ErrInvalidInput
			}
			r.Taxonomy[j] = redact.Free(taxonomy)
			if !policyText(r.Taxonomy[j], 64) {
				return nil, ErrInvalidInput
			}
		}
		slices.Sort(r.Taxonomy)
		r.Taxonomy = slices.Compact(r.Taxonomy)
		if r.Confidence != nil {
			c := *r.Confidence
			if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 || c > 1 {
				return nil, ErrInvalidInput
			}
			r.Confidence = &c
		}
		out[i] = r
	}
	return out, nil
}

// CheckAnnotationAuthority runs inside the store transaction so policy/session
// revocation cannot race the annotation write.
func CheckAnnotationAuthority(ctx context.Context, st Store, record RepositoryAnnotation) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	session, err := st.ExecutionSessionByID(ctx, id.SessionID)
	if err != nil || session.UserID != id.SubjectID || session.OrgID != id.TenantID || !session.Active(time.Now()) {
		return ErrExecutionSessionClosed
	}
	u, err := st.UserByID(ctx, id.SubjectID)
	if err != nil {
		return err
	}
	svc := NewService(st, false)
	if _, err := svc.ResolveRepository(ctx, Principal{User: u, Via: "session", OrgID: id.TenantID}, record.OrgID, record.Graph, Member); err != nil {
		return err
	}
	policy, err := st.GetRepoPolicy(ctx, record.OrgID, record.Graph)
	if err != nil || !policy.AllowAnnotation || policy.Digest != record.PolicyDigest || (policy.ExpiresAt != nil && !policy.ExpiresAt.After(time.Now())) {
		return ErrForbidden
	}
	return nil
}

func policyText(s string, limit int) bool {
	return len(s) > 0 && len(s) <= limit && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}
