package auditarchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PromptLogFileRows bounds the rows of one prompt log file: each row may
// carry three texts of accounts.MaxPromptLogText.
const PromptLogFileRows = 500

func promptLogKey(first, last int64) string {
	return fmt.Sprintf("promptlog-%020d-%020d.jsonl.gz", first, last)
}

// PromptLogResult says what one retention pass did.
type PromptLogResult struct {
	Pruned, Sealed, Expired int
}

// PromptLogRetention applies each organisation's prompt log retention.
// Rows past the retention are deleted. With an archive, rows older than hot
// (and still inside retention) are first sealed into files whose SHA-256 is
// recorded in the same transaction that deletes the rows, and files go once
// their newest row passes the retention. Without an archive rows simply stay
// in PostgreSQL until their retention ends. It runs under the api-loops
// lease; every organisation is attempted and the first error is returned.
func PromptLogRetention(ctx context.Context, st accounts.Store, arch Archive, hot time.Duration, now time.Time) (PromptLogResult, error) {
	var res PromptLogResult
	orgs, err := st.PromptLogOrgs(ctx)
	if err != nil {
		return res, fmt.Errorf("prompt log retention: %w", err)
	}
	var errs []error
	for org, days := range orgs {
		retention := time.Duration(days) * 24 * time.Hour
		n, err := st.PrunePromptLog(ctx, org, now.Add(-retention))
		res.Pruned += n
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if arch == nil || hot <= 0 || hot >= retention {
			continue
		}
		segs, err := SealPromptLog(ctx, st, arch, org, now.Add(-hot), retention, now)
		res.Sealed += len(segs)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if arch != nil {
		n, err := ExpirePromptLog(ctx, st, arch, now)
		res.Expired += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// SealPromptLog moves an organisation's rows older than before into archive
// files, oldest first, at most PromptLogFileRows per file. Each file is read
// back and compared before the database deletes the rows; the database then
// checks they are still exactly the rows written (accounts.ErrAuditConflict).
// Files are locked until their newest row is retention old.
func SealPromptLog(ctx context.Context, st accounts.Store, arch Archive, orgID string, before time.Time, retention time.Duration, now time.Time) ([]accounts.PromptLogSegment, error) {
	var out []accounts.PromptLogSegment
	for {
		rows, err := st.ListPromptLog(ctx, accounts.PromptLogFilter{OrgID: orgID, Until: before, Ascending: true, Limit: PromptLogFileRows})
		if err != nil || len(rows) == 0 {
			return out, err
		}
		body, err := encode(rows)
		if err != nil {
			return out, err
		}
		first, last := rows[0], rows[len(rows)-1]
		seg := accounts.PromptLogSegment{OrgID: orgID, FirstID: first.ID, LastID: last.ID, FirstAt: first.At, LastAt: first.At, Count: len(rows)}
		for _, r := range rows {
			seg.FirstAt, seg.LastAt = minTime(seg.FirstAt, r.At), maxTime(seg.LastAt, r.At)
		}
		sum := sha256.Sum256(body)
		seg.SHA256, seg.Bytes, seg.Backend, seg.Key, seg.SealedAt = hex.EncodeToString(sum[:]), int64(len(body)), arch.Backend(), promptLogKey(seg.FirstID, seg.LastID), now.UTC()
		if err := arch.Put(ctx, seg.Key, body, seg.LastAt.Add(retention)); err != nil {
			return out, fmt.Errorf("prompt log seal: store %s: %w", seg.Key, err)
		}
		back, err := arch.Get(ctx, seg.Key)
		if err != nil {
			return out, fmt.Errorf("prompt log seal: read back %s: %w", seg.Key, err)
		}
		if !bytes.Equal(back, body) {
			return out, fmt.Errorf("prompt log seal: %s reads back different from what was written", seg.Key)
		}
		seg, err = st.SealPromptLogSegment(ctx, seg)
		if err != nil {
			return out, fmt.Errorf("prompt log seal: %w", err)
		}
		out = append(out, seg)
		if len(rows) < PromptLogFileRows {
			return out, nil
		}
	}
}

// ExpirePromptLog removes prompt log files whose newest row is past its
// organisation's current retention: the file first, then the row. A file
// that cannot be removed yet (an object lock from a longer retention) is
// left for a later pass.
func ExpirePromptLog(ctx context.Context, st accounts.Store, arch Archive, now time.Time) (int, error) {
	segs, err := st.PromptLogSegments(ctx, now.Add(-time.Duration(accounts.MinPromptLogDays)*24*time.Hour))
	if err != nil {
		return 0, fmt.Errorf("prompt log expire: %w", err)
	}
	days := map[string]int{}
	var n int
	var errs []error
	for _, seg := range segs {
		d, ok := days[seg.OrgID]
		if !ok {
			set, err := st.PromptLogSetting(ctx, seg.OrgID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			d, days[seg.OrgID] = set.RetentionDays, set.RetentionDays
		}
		if !seg.LastAt.Before(now.Add(-time.Duration(d) * 24 * time.Hour)) {
			continue
		}
		if seg.Backend != arch.Backend() {
			errs = append(errs, fmt.Errorf("prompt log expire: segment %d is in %s, not %s", seg.ID, seg.Backend, arch.Backend()))
			continue
		}
		if err := arch.Delete(ctx, seg.Key); err != nil {
			errs = append(errs, fmt.Errorf("prompt log expire: %w", err))
			continue
		}
		if err := st.DropPromptLogSegment(ctx, seg.ID); err != nil {
			errs = append(errs, fmt.Errorf("prompt log expire: %w", err))
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

// DecodePromptLog reads a prompt log file back (bounded like decode), for
// an operator checking a file against its recorded SHA-256 and the chain.
func DecodePromptLog(body []byte) ([]accounts.PromptLogEntry, error) {
	// Three texts of MaxPromptLogText each, JSON-escaped, fit in a line.
	return decodeLines[accounts.PromptLogEntry](body, PromptLogFileRows, 8<<20)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
