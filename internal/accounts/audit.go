package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Outcomes.
const (
	Success = "success"
	Failure = "failure"
)

// DefaultRetention keeps audit entries a little over a year.
const DefaultRetention = 400 * 24 * time.Hour

// GenesisHash is the PrevHash of the first entry.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ChainHash is SHA-256 over PrevHash and the entry's fields (not its ID,
// which the database assigns, nor Hash itself).
func (e Event) ChainHash() string {
	canon, _ := json.Marshal([]any{ //nolint:errcheck // plain values always marshal
		e.PrevHash, e.At.UTC().Format(time.RFC3339Nano), e.RequestID, e.UserID, e.ActorEmail, e.Via, e.KeyID,
		e.OrgID, e.IP, e.UserAgent, e.Action, e.Target, e.Outcome, e.Status, e.Detail,
	})
	h := sha256.Sum256(canon)
	return hex.EncodeToString(h[:])
}

// clean caps free-text fields so a hostile client cannot bloat the log.
func (e *Event) clean() {
	clip := func(s string, n int) string {
		s = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, s)
		if r := []rune(s); len(r) > n {
			return string(r[:n])
		}
		return s
	}
	e.RequestID, e.UserAgent, e.Target, e.Detail = clip(e.RequestID, 64), clip(e.UserAgent, 200), clip(e.Target, 300), clip(e.Detail, 500)
	e.Action, e.ActorEmail, e.IP = clip(e.Action, 120), clip(e.ActorEmail, 254), clip(e.IP, 64)
	if e.Outcome == "" {
		e.Outcome = Success
	}
	e.At = e.At.UTC().Truncate(time.Microsecond)
}

// Prepare cleans e for appending after prev (the last entry's hash, or "").
func (e Event) Prepare(prev string) Event {
	e.clean()
	if prev == "" {
		prev = GenesisHash
	}
	e.PrevHash = prev
	e.Hash = e.ChainHash()
	return e
}

// Verification is the result of checking the chain.
type Verification struct {
	Checked int  `json:"checked"`
	OK      bool `json:"ok"`
	// BrokenAt is the first entry whose hash or link does not match.
	BrokenAt int64  `json:"broken_at,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// FirstID is where verification started: older entries were pruned and
	// the first remaining entry's PrevHash is trusted as the anchor.
	FirstID int64 `json:"first_id,omitempty"`
}

// VerifyAudit walks the whole chain oldest first.
func VerifyAudit(ctx context.Context, st Store) (Verification, error) {
	var v Verification
	prev := ""
	var after int64
	for {
		page, err := st.ListEvents(ctx, EventFilter{AfterID: after, Limit: 1000, Ascending: true})
		if err != nil {
			return v, err
		}
		for _, e := range page {
			if v.FirstID == 0 {
				v.FirstID = e.ID
			} else if e.PrevHash != prev {
				v.BrokenAt, v.Reason = e.ID, "link to the previous entry does not match (an entry was removed or changed)"
				return v, nil
			}
			if e.ChainHash() != e.Hash {
				v.BrokenAt, v.Reason = e.ID, "entry content does not match its hash (it was changed)"
				return v, nil
			}
			prev, after = e.Hash, e.ID
			v.Checked++
		}
		if len(page) < 1000 {
			v.OK = true
			return v, nil
		}
	}
}

// RequestInfo describes the HTTP request an action comes from.
type RequestInfo struct {
	RequestID string
	IP        string
	UserAgent string
}

type requestInfoKey struct{}

// WithRequestInfo attaches request details for the audit log.
func WithRequestInfo(ctx context.Context, ri RequestInfo) context.Context {
	return context.WithValue(ctx, requestInfoKey{}, ri)
}

// RequestInfoFrom returns the request details attached to ctx.
func RequestInfoFrom(ctx context.Context) RequestInfo {
	ri, _ := ctx.Value(requestInfoKey{}).(RequestInfo)
	return ri
}

// Sink receives every audit entry after it is stored (structured logs for
// a SIEM). It must not block.
type Sink func(Event)

// Record appends an entry, filling request details from ctx, and hands it to
// the service's sink. Errors are returned so callers can decide; the
// service's own events never fail an action.
func (s *Service) Record(ctx context.Context, e Event) (Event, error) {
	ri := RequestInfoFrom(ctx)
	if e.RequestID == "" {
		e.RequestID = ri.RequestID
	}
	if e.IP == "" {
		e.IP = ri.IP
	}
	if e.UserAgent == "" {
		e.UserAgent = ri.UserAgent
	}
	if e.At.IsZero() {
		e.At = s.now()
	}
	stored, err := s.St.AddEvent(ctx, e)
	if err != nil {
		return e, fmt.Errorf("audit log: %w", err)
	}
	if s.Sink != nil {
		s.Sink(stored)
	}
	return stored, nil
}

// actorEvent fills the actor from a principal.
func actorEvent(p Principal, action, orgID, target string) Event {
	return Event{UserID: p.User.ID, ActorEmail: p.User.Email, Via: p.Via, KeyID: p.KeyID, OrgID: orgID, Action: action, Target: target}
}
