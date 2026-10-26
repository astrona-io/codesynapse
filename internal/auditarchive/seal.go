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

// Seal moves every entry older than before into archive files, oldest
// first, at most SegmentEntries per file. Each file is read back and
// compared before the database forgets the entries; the database then
// checks they are still the oldest and unchanged (accounts.ErrAuditConflict).
// retention sets how long the files are locked.
func Seal(ctx context.Context, st accounts.Store, arch Archive, before time.Time, retention time.Duration, now time.Time) ([]accounts.AuditSegment, error) {
	var out []accounts.AuditSegment
	for {
		events, err := oldest(ctx, st, before)
		if err != nil || len(events) == 0 {
			return out, err
		}
		seg := describe(events)
		if err := accounts.CheckSegment(seg, events); err != nil {
			// A broken chain is not sealed: verification reports it and
			// the entries stay where an operator can look at them.
			return out, fmt.Errorf("audit seal: %w", err)
		}
		body, err := encode(events)
		if err != nil {
			return out, err
		}
		sum := sha256.Sum256(body)
		seg.SHA256, seg.Bytes, seg.Backend, seg.Key, seg.SealedAt = hex.EncodeToString(sum[:]), int64(len(body)), arch.Backend(), segmentKey(seg.FirstID, seg.LastID), now.UTC()
		if err := arch.Put(ctx, seg.Key, body, seg.LastAt.Add(retention)); err != nil {
			return out, fmt.Errorf("audit seal: store %s: %w", seg.Key, err)
		}
		back, err := arch.Get(ctx, seg.Key)
		if err != nil {
			return out, fmt.Errorf("audit seal: read back %s: %w", seg.Key, err)
		}
		if !bytes.Equal(back, body) {
			return out, fmt.Errorf("audit seal: %s reads back different from what was written", seg.Key)
		}
		seg, err = st.SealAuditSegment(ctx, seg)
		if err != nil {
			// The file stays: a later seal of the same entries replaces it,
			// and a locked one cannot be removed anyway.
			return out, fmt.Errorf("audit seal: %w", err)
		}
		out = append(out, seg)
	}
}

// oldest returns up to SegmentEntries of the oldest entries, stopping at the
// first one at or after before. Entries are taken in chain (ID) order, so a
// segment is always a contiguous piece of the chain even where clocks made
// a later entry a little older.
func oldest(ctx context.Context, st accounts.Store, before time.Time) ([]accounts.Event, error) {
	var out []accounts.Event
	var after int64
	for len(out) < SegmentEntries {
		page, err := st.ListEvents(ctx, accounts.EventFilter{AfterID: after, Limit: min(1000, SegmentEntries-len(out)), Ascending: true})
		if err != nil {
			return nil, err
		}
		for _, e := range page {
			if !e.At.Before(before) {
				return out, nil
			}
			out = append(out, e)
			after = e.ID
		}
		if len(page) == 0 {
			break
		}
	}
	return out, nil
}

func describe(events []accounts.Event) accounts.AuditSegment {
	first, last := events[0], events[len(events)-1]
	seg := accounts.AuditSegment{FirstID: first.ID, LastID: last.ID, FirstAt: first.At, LastAt: last.At, Count: len(events),
		PrevHash: first.PrevHash, LastHash: last.Hash}
	for _, e := range events {
		if e.At.Before(seg.FirstAt) {
			seg.FirstAt = e.At
		}
		if e.At.After(seg.LastAt) {
			seg.LastAt = e.At
		}
	}
	return seg
}

// Expire removes the oldest segments whose newest entry is older than
// before (the end of retention): the file first, then the row.
func Expire(ctx context.Context, st accounts.Store, arch Archive, before time.Time) ([]accounts.AuditSegment, error) {
	segs, err := st.AuditSegments(ctx)
	if err != nil {
		return nil, err
	}
	var out []accounts.AuditSegment
	for _, seg := range segs {
		if !seg.LastAt.Before(before) {
			break
		}
		if seg.Backend != arch.Backend() {
			return out, fmt.Errorf("audit expire: segment %d is in %s, not %s", seg.ID, seg.Backend, arch.Backend())
		}
		if err := arch.Delete(ctx, seg.Key); err != nil {
			return out, fmt.Errorf("audit expire: %w", err)
		}
		if err := st.DropAuditSegment(ctx, seg.ID); err != nil {
			return out, fmt.Errorf("audit expire: %w", err)
		}
		out = append(out, seg)
	}
	return out, nil
}

// DeepVerify checks the chain like accounts.VerifyAudit and then reads every
// archive file back, checking its checksum and each entry in it.
func DeepVerify(ctx context.Context, st accounts.Store, arch Archive) (accounts.Verification, error) {
	v, err := accounts.VerifyAudit(ctx, st)
	if err != nil || !v.OK {
		return v, err
	}
	segs, err := st.AuditSegments(ctx)
	if err != nil {
		return v, err
	}
	for _, seg := range segs {
		if err := checkFile(ctx, arch, seg); err != nil {
			if ctx.Err() != nil {
				return v, ctx.Err()
			}
			v.OK, v.BrokenAt, v.Reason = false, seg.FirstID, err.Error()
			return v, nil
		}
		v.Archives++
	}
	return v, nil
}

func checkFile(ctx context.Context, arch Archive, seg accounts.AuditSegment) error {
	if seg.Backend != arch.Backend() {
		return fmt.Errorf("archive of segment %d is in %s, not %s", seg.ID, seg.Backend, arch.Backend())
	}
	body, err := arch.Get(ctx, seg.Key)
	if err != nil {
		return fmt.Errorf("archive of segment %d cannot be read", seg.ID)
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != seg.SHA256 || int64(len(body)) != seg.Bytes {
		return fmt.Errorf("archive of segment %d does not match its checksum (it was changed)", seg.ID)
	}
	events, err := decode(body)
	if err != nil {
		return fmt.Errorf("archive of segment %d cannot be decoded", seg.ID)
	}
	if err := accounts.CheckSegment(seg, events); err != nil {
		return errors.New("archive " + err.Error())
	}
	return nil
}
