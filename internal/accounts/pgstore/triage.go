package pgstore

import (
	"context"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PutFindingTriage implements accounts.Store. The service calls it inside
// a policy transaction, which serialises the bound check with the insert.
func (s *Store) PutFindingTriage(ctx context.Context, t accounts.FindingTriage) error {
	p, err := accounts.PrepareFindingTriage(t)
	if err != nil {
		return err
	}
	var count int
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT count(*), coalesce(bool_or(fingerprint=$3), false) FROM finding_triage WHERE org_id=$1 AND graph=$2`, p.OrgID, p.Graph, p.Fingerprint).Scan(&count, &exists); err != nil {
		return err
	}
	if !exists && count >= accounts.MaxFindingTriage {
		return accounts.ErrAccessLimit
	}
	_, err = s.db.Exec(ctx, `INSERT INTO finding_triage (org_id,graph,fingerprint,rule,file,state,reference,reason,actor_id,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
ON CONFLICT (org_id,graph,fingerprint) DO UPDATE SET rule=EXCLUDED.rule,file=EXCLUDED.file,state=EXCLUDED.state,reference=EXCLUDED.reference,reason=EXCLUDED.reason,actor_id=EXCLUDED.actor_id,created_at=EXCLUDED.created_at,expires_at=EXCLUDED.expires_at`,
		p.OrgID, p.Graph, p.Fingerprint, p.Rule, p.File, p.State, p.Reference, p.Reason, p.ActorID, p.CreatedAt, p.ExpiresAt)
	return mapErr(err, "finding triage")
}

// DeleteFindingTriage implements accounts.Store.
func (s *Store) DeleteFindingTriage(ctx context.Context, orgID, graph, fingerprint string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM finding_triage WHERE org_id=$1 AND graph=$2 AND fingerprint=$3`, orgID, graph, fingerprint)
	return affected(tag, err, "finding triage")
}

// ListFindingTriage implements accounts.Store.
func (s *Store) ListFindingTriage(ctx context.Context, orgID, graph string, limit int) ([]accounts.FindingTriage, error) {
	rows, err := s.db.Query(ctx, `SELECT t.org_id,t.graph,t.fingerprint,t.rule,t.file,t.state,t.reference,t.reason,t.actor_id,u.name,t.created_at,t.expires_at
FROM finding_triage t JOIN users u ON u.id=t.actor_id WHERE t.org_id=$1 AND t.graph=$2 ORDER BY t.created_at DESC, t.fingerprint LIMIT $3`,
		orgID, graph, min(max(limit, 1), accounts.MaxFindingTriage))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []accounts.FindingTriage{}
	for rows.Next() {
		var t accounts.FindingTriage
		if err := rows.Scan(&t.OrgID, &t.Graph, &t.Fingerprint, &t.Rule, &t.File, &t.State, &t.Reference, &t.Reason, &t.ActorID, &t.ActorName, &t.CreatedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
