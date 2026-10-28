package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Outcomes.
const (
	Success = "success"
	Failure = "failure"
)

// Bounds on the arguments an MCP request leaves in the audit log.
const (
	MaxMCPArgs     = 32
	MaxMCPArgValue = 400
)

// DefaultRetention keeps audit entries a little over a year.
const DefaultRetention = 400 * 24 * time.Hour

// DefaultHot is how long entries stay in the database when an archive takes
// the older ones.
const DefaultHot = 90 * 24 * time.Hour

// GenesisHash is the PrevHash of the first entry.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ChainHash is SHA-256 over PrevHash and the entry's fields (not its ID,
// which the database assigns, nor Hash itself). MCP details join the list
// only when present, so entries written before they existed still verify.
func (e Event) ChainHash() string {
	fields := []any{
		e.PrevHash, e.At.UTC().Format(time.RFC3339Nano), e.RequestID, e.UserID, e.ActorEmail, e.Via, e.KeyID,
		e.OrgID, e.IP, e.UserAgent, e.Action, e.Target, e.Outcome, e.Status, e.Detail,
	}
	if e.MCP != nil {
		fields = append(fields, e.MCP)
	}
	canon, _ := json.Marshal(fields) //nolint:errcheck // plain values always marshal
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
	if m := e.MCP; m != nil {
		c := *m
		c.Method, c.Name, c.Repo = clip(c.Method, 64), clip(c.Name, 120), clip(c.Repo, 300)
		c.SessionID, c.TransportID, c.Client = clip(c.SessionID, 128), clip(c.TransportID, 128), clip(c.Client, 200)
		c.ArgsDigest, c.Error = clip(c.ArgsDigest, 120), clip(c.Error, 300)
		if len(c.Args) > 0 {
			args := make(map[string]string, min(len(c.Args), MaxMCPArgs))
			for _, k := range slices.Sorted(maps.Keys(c.Args)) {
				if len(args) == MaxMCPArgs {
					break
				}
				args[clip(k, 64)] = clip(c.Args[k], MaxMCPArgValue)
			}
			c.Args = args
		}
		e.MCP = &c
	}
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
	// the first remaining entry's (or segment's) PrevHash is trusted as the
	// anchor.
	FirstID int64 `json:"first_id,omitempty"`
	// Segments and Sealed count the archived segments and the entries in
	// them, linked through the hashes the database keeps; Archives counts the
	// files that were read and checked entry by entry (a deep check).
	Segments int `json:"segments,omitempty"`
	Sealed   int `json:"sealed,omitempty"`
	Archives int `json:"archives,omitempty"`
}

// VerifyAudit walks the whole chain oldest first: the sealed segments by
// their stored hashes, then every entry still in the database.
func VerifyAudit(ctx context.Context, st Store) (Verification, error) {
	v, boundary, err := verifyAudit(ctx, st)
	if err == nil && boundary {
		// A seal between reading the segments and the entries moves the
		// boundary; one more pass sees a consistent picture.
		v, _, err = verifyAudit(ctx, st)
	}
	return v, err
}

func verifyAudit(ctx context.Context, st Store) (v Verification, boundary bool, err error) {
	segs, err := st.AuditSegments(ctx)
	if err != nil {
		return v, false, err
	}
	prev := ""
	for i, sg := range segs {
		if i == 0 {
			v.FirstID = sg.FirstID
		} else if sg.PrevHash != prev {
			v.BrokenAt, v.Reason = sg.FirstID, "archived segment does not link to the one before it"
			return v, false, nil
		}
		prev = sg.LastHash
		v.Segments++
		v.Sealed += sg.Count
	}
	var after int64
	for {
		page, err := st.ListEvents(ctx, EventFilter{AfterID: after, Limit: 1000, Ascending: true})
		if err != nil {
			return v, false, err
		}
		for _, e := range page {
			if v.FirstID == 0 {
				v.FirstID = e.ID
			} else if e.PrevHash != prev {
				v.BrokenAt, v.Reason = e.ID, "link to the previous entry does not match (an entry was removed or changed)"
				return v, v.Checked == 0 && len(segs) > 0, nil
			}
			if e.ChainHash() != e.Hash {
				v.BrokenAt, v.Reason = e.ID, "entry content does not match its hash (it was changed)"
				return v, false, nil
			}
			prev, after = e.Hash, e.ID
			v.Checked++
		}
		if len(page) < 1000 {
			v.OK = true
			return v, false, nil
		}
	}
}

// CheckSegment checks entries read back from an archive file against the
// segment that describes them: their count, IDs, hashes and links.
func CheckSegment(seg AuditSegment, events []Event) error {
	if len(events) != seg.Count || len(events) == 0 {
		return fmt.Errorf("segment %d: %d entries, expected %d", seg.ID, len(events), seg.Count)
	}
	prev := seg.PrevHash
	for i, e := range events {
		if e.PrevHash != prev {
			return fmt.Errorf("segment %d: entry %d does not link to the one before it", seg.ID, e.ID)
		}
		if e.ChainHash() != e.Hash {
			return fmt.Errorf("segment %d: entry %d does not match its hash", seg.ID, e.ID)
		}
		if i > 0 && e.ID <= events[i-1].ID {
			return fmt.Errorf("segment %d: entry %d is out of order", seg.ID, e.ID)
		}
		prev = e.Hash
	}
	if events[0].ID != seg.FirstID || events[len(events)-1].ID != seg.LastID || prev != seg.LastHash {
		return fmt.Errorf("segment %d: entries %d-%d do not match the recorded %d-%d", seg.ID, events[0].ID, events[len(events)-1].ID, seg.FirstID, seg.LastID)
	}
	return nil
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
	start := time.Now()
	stored, err := s.St.AddEvent(ctx, e)
	// Counted for the tenant and source ctx carries, never e.OrgID: the
	// event's organisation may be the target of a refused request.
	telemetry.RecordAuditWrite(ctx, time.Since(start), err == nil)
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
