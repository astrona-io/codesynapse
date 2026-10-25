package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/imagescan"
)

// ImageScan implements accounts.Store.
func (s *Store) ImageScan(ctx context.Context, ns string) (accounts.ImageScan, bool, error) {
	var (
		at  time.Time
		raw []byte
	)
	err := s.db.QueryRow(ctx, "SELECT uploaded_at, findings FROM image_scans WHERE ns = $1", ns).Scan(&at, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.ImageScan{}, false, nil
	}
	if err != nil {
		return accounts.ImageScan{}, false, err
	}
	sc := accounts.ImageScan{UploadedAt: at, Findings: []imagescan.Finding{}}
	if err := json.Unmarshal(raw, &sc.Findings); err != nil {
		return accounts.ImageScan{}, false, err
	}
	return sc, true, nil
}

// SetImageScan implements accounts.Store.
func (s *Store) SetImageScan(ctx context.Context, ns string, sc accounts.ImageScan) error {
	raw, err := json.Marshal(sc.Findings)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO image_scans (ns, uploaded_at, findings) VALUES ($1, $2, $3)
ON CONFLICT (ns) DO UPDATE SET uploaded_at = EXCLUDED.uploaded_at, findings = EXCLUDED.findings`, ns, sc.UploadedAt, raw)
	return err
}

// DeleteImageScan implements accounts.Store.
func (s *Store) DeleteImageScan(ctx context.Context, ns string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM image_scans WHERE ns = $1", ns)
	return err
}
