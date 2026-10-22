package pgstore

import (
	"context"
	"encoding/json"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// GetRepoPolicy returns one tenant's exact configuration approval.
func (s *Store) GetRepoPolicy(ctx context.Context, orgID, graph string) (accounts.RepoPolicyApproval, error) {
	var p accounts.RepoPolicyApproval
	err := s.db.QueryRow(ctx, `SELECT org_id,graph,digest,config_json,reason,actor_id,allow_exclusions,allow_suppressions,allow_annotation,updated_at,expires_at FROM repository_policy_approvals WHERE org_id=$1 AND graph=$2`, orgID, graph).Scan(&p.OrgID, &p.Graph, &p.Digest, &p.ConfigJSON, &p.Reason, &p.ActorID, &p.AllowExclusions, &p.AllowSuppressions, &p.AllowAnnotation, &p.UpdatedAt, &p.ExpiresAt)
	return p, mapErr(err, "repository policy")
}

// PutRepoPolicy replaces a tenant's approval; foreign keys fence its scope.
func (s *Store) PutRepoPolicy(ctx context.Context, approval accounts.RepoPolicyApproval) error {
	p, err := accounts.PrepareRepoPolicy(approval)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO repository_policy_approvals (org_id,graph,digest,config_json,reason,actor_id,allow_exclusions,allow_suppressions,allow_annotation,updated_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT (org_id,graph) DO UPDATE SET digest=EXCLUDED.digest,config_json=EXCLUDED.config_json,reason=EXCLUDED.reason,actor_id=EXCLUDED.actor_id,allow_exclusions=EXCLUDED.allow_exclusions,allow_suppressions=EXCLUDED.allow_suppressions,allow_annotation=EXCLUDED.allow_annotation,updated_at=EXCLUDED.updated_at,expires_at=EXCLUDED.expires_at`, p.OrgID, p.Graph, p.Digest, p.ConfigJSON, p.Reason, p.ActorID, p.AllowExclusions, p.AllowSuppressions, p.AllowAnnotation, p.UpdatedAt, p.ExpiresAt)
	return mapErr(err, "repository policy")
}

// DeleteRepoPolicy revokes approval while retaining annotation provenance.
func (s *Store) DeleteRepoPolicy(ctx context.Context, orgID, graph string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM repository_policy_approvals WHERE org_id=$1 AND graph=$2`, orgID, graph)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return accounts.ErrNotFound
	}
	return nil
}

// UpsertAnnotations serializes the quota and authority checks with account policy
// changes. A repeated batch preserves its first committed record.
func (s *Store) UpsertAnnotations(ctx context.Context, records []accounts.RepositoryAnnotation) error {
	prepared, err := accounts.PrepareAnnotations(ctx, records)
	if err != nil {
		return err
	}
	first := prepared[0]
	return s.PolicyTransaction(ctx, first.OrgID, func(st accounts.Store) error {
		if err := accounts.CheckAnnotationAuthority(ctx, st, first); err != nil {
			return err
		}
		tx := st.(*Store)
		ids := make([]string, 0, len(prepared))
		for _, r := range prepared {
			ids = append(ids, r.SymbolID)
		}
		var additional int
		if err := tx.db.QueryRow(ctx, `SELECT count(*) FROM unnest($3::text[]) AS candidate WHERE NOT EXISTS (SELECT 1 FROM repository_annotations WHERE org_id=$1 AND graph=$2 AND symbol_id=candidate)`, first.OrgID, first.Graph, ids).Scan(&additional); err != nil {
			return err
		}
		var count int
		if err := tx.db.QueryRow(ctx, `SELECT count(*) FROM repository_annotations WHERE org_id=$1 AND graph=$2`, first.OrgID, first.Graph).Scan(&count); err != nil {
			return err
		}
		if count+additional > accounts.MaxRepositoryAnnotations {
			return accounts.ErrAccessLimit
		}
		for _, r := range prepared {
			taxonomy, err := json.Marshal(r.Taxonomy)
			if err != nil {
				return err
			}
			_, err = tx.db.Exec(ctx, `INSERT INTO repository_annotations (org_id,graph,symbol_id,commit_id,revision,fingerprint,policy_digest,job_id,origin_session_id,summary,taxonomy,confidence,updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT (org_id,graph,symbol_id) DO UPDATE SET commit_id=EXCLUDED.commit_id,revision=EXCLUDED.revision,fingerprint=EXCLUDED.fingerprint,policy_digest=EXCLUDED.policy_digest,job_id=EXCLUDED.job_id,origin_session_id=EXCLUDED.origin_session_id,summary=EXCLUDED.summary,taxonomy=EXCLUDED.taxonomy,confidence=EXCLUDED.confidence,updated_at=EXCLUDED.updated_at WHERE repository_annotations.job_id<>EXCLUDED.job_id AND repository_annotations.updated_at<=EXCLUDED.updated_at`, r.OrgID, r.Graph, r.SymbolID, r.Commit, r.Revision, r.Fingerprint, r.PolicyDigest, r.JobID, r.OriginSessionID, r.Summary, taxonomy, r.Confidence, r.UpdatedAt)
			if err != nil {
				return mapErr(err, "annotation")
			}
		}
		return nil
	})
}

// ListAnnotations returns a bounded tenant-filtered result in symbol order.
func (s *Store) ListAnnotations(ctx context.Context, orgID, graph string, limit int) ([]accounts.RepositoryAnnotation, error) {
	rows, err := s.db.Query(ctx, `SELECT org_id,graph,symbol_id,commit_id,revision,fingerprint,policy_digest,job_id,origin_session_id,summary,taxonomy,confidence,updated_at FROM repository_annotations WHERE org_id=$1 AND graph=$2 ORDER BY symbol_id LIMIT $3`, orgID, graph, min(max(limit, 1), accounts.MaxRepositoryAnnotations))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []accounts.RepositoryAnnotation{}
	for rows.Next() {
		var r accounts.RepositoryAnnotation
		var taxonomy []byte
		if err := rows.Scan(&r.OrgID, &r.Graph, &r.SymbolID, &r.Commit, &r.Revision, &r.Fingerprint, &r.PolicyDigest, &r.JobID, &r.OriginSessionID, &r.Summary, &taxonomy, &r.Confidence, &r.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(taxonomy, &r.Taxonomy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
