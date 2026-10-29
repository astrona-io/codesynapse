package accounts

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/redact"
)

// The gateway prompt log is the one place CodeSynapse keeps what an agent
// said to a model. It is off unless an owner or admin of the organisation
// turns it on from a signed-in session; then each gateway request keeps the
// newest user text, the shortened text the gateway forwarded (if any) and
// the local model's answer (if it answered), each redacted and capped, for
// the organisation's retention period. The audit chain itself never holds
// text: its gateway.request entry carries SHA-256 digests, which every row
// repeats so the two can be matched. Tool results and system prompts are
// never kept. Rows are readable only through PromptLog, never by an API key.

// Bounds of the prompt log.
const (
	// MaxPromptLogText caps each stored text after redaction.
	MaxPromptLogText = 64 << 10
	// PromptLogCapture is how much of a text the gateway hands over: twice
	// the stored cap, so a secret cut at the capture edge is redacted or
	// falls beyond the stored cap.
	PromptLogCapture = 2 * MaxPromptLogText
	// DefaultPromptLogDays is the retention of an organisation that never chose one.
	DefaultPromptLogDays = 30
	MinPromptLogDays     = 1
	MaxPromptLogDays     = 400
	// MaxPromptLogPage bounds one read of the log.
	MaxPromptLogPage = 500
	// MaxPromptLogSegments bounds the segment list read at once.
	MaxPromptLogSegments = 5000
)

// Prompt log audit actions.
const (
	ActionPromptLogSet    = "org.gateway.prompt_log.set"
	ActionPromptLogRead   = "gateway.prompt_log.read"
	ActionPromptLogExport = "gateway.prompt_log.export"
)

// PromptLogSetting is an organisation's choice: off with DefaultPromptLogDays
// until someone changes it.
type PromptLogSetting struct {
	Enabled       bool `json:"enabled"`
	RetentionDays int  `json:"retention_days"`
}

// DefaultPromptLogSetting is the setting of an organisation that never chose.
func DefaultPromptLogSetting() PromptLogSetting {
	return PromptLogSetting{RetentionDays: DefaultPromptLogDays}
}

// PromptLogEntry is one gateway request in the prompt log. AuditID links it
// to its gateway.request entry, whose detail carries the same digests.
type PromptLogEntry struct {
	ID          int64     `json:"id"`
	OrgID       string    `json:"org_id"`
	At          time.Time `json:"at"`
	AuditID     int64     `json:"audit_id,omitempty"`
	UserID      string    `json:"user_id,omitempty"`
	KeyID       string    `json:"key_id,omitempty"`
	SessionID   string    `json:"session_id,omitempty"`
	Path        string    `json:"path"`
	Model       string    `json:"model,omitempty"`
	Route       string    `json:"route"` // local, cloud or rejected
	Status      int       `json:"status"`
	DurationMS  int64     `json:"duration_ms"`
	Shortened   bool      `json:"shortened"`
	Fallthrough string    `json:"fallthrough,omitempty"`
	// Digests and sizes are of the text the agent sent and the text the
	// gateway forwarded, before redaction and capping.
	PromptSHA256    string `json:"prompt_sha256,omitempty"`
	PromptBytes     int    `json:"prompt_bytes"`
	ForwardedSHA256 string `json:"forwarded_sha256,omitempty"`
	ForwardedBytes  int    `json:"forwarded_bytes"`
	// Prompt, Forwarded and Answer are redacted and capped at MaxPromptLogText.
	Prompt    string `json:"prompt,omitempty"`
	Forwarded string `json:"forwarded,omitempty"`
	Answer    string `json:"answer,omitempty"`
}

// PromptLogFilter selects rows of one organisation. Zero values mean no
// bound. Rows come newest first, or oldest first with Ascending; BeforeID and
// AfterID page through them.
type PromptLogFilter struct {
	OrgID                                     string
	Since, Until                              time.Time
	UserID, SessionID, Model, Route, Fallthru string
	Shortened                                 *bool
	BeforeID, AfterID                         int64
	Limit                                     int
	Ascending                                 bool
}

// PromptLogSegment is a run of an organisation's oldest prompt log rows
// sealed into an archive file and removed from the database.
type PromptLogSegment struct {
	ID       int64     `json:"id"`
	OrgID    string    `json:"org_id"`
	FirstID  int64     `json:"first_id"`
	LastID   int64     `json:"last_id"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
	Count    int       `json:"count"`
	SHA256   string    `json:"sha256"`
	Bytes    int64     `json:"bytes"`
	Backend  string    `json:"backend"`
	Key      string    `json:"key"`
	SealedAt time.Time `json:"sealed_at"`
}

// PromptLogStore keeps the prompt log apart from the audit chain.
type PromptLogStore interface {
	// PromptLogSetting returns DefaultPromptLogSetting when the organisation
	// never chose; SetPromptLogSetting records a choice (ErrNotFound for an
	// unknown organisation).
	PromptLogSetting(ctx context.Context, orgID string) (PromptLogSetting, error)
	SetPromptLogSetting(ctx context.Context, orgID string, s PromptLogSetting) error
	// AddPromptLog stores a row and returns it with its ID.
	AddPromptLog(ctx context.Context, e PromptLogEntry) (PromptLogEntry, error)
	// ListPromptLog reads rows of f.OrgID (required) matching f.
	ListPromptLog(ctx context.Context, f PromptLogFilter) ([]PromptLogEntry, error)
	// PromptLogOrgs lists the organisations that have rows, with their
	// retention, so pruning reaches organisations that switched it off.
	PromptLogOrgs(ctx context.Context) (map[string]int, error)
	// PrunePromptLog deletes an organisation's rows older than before.
	PrunePromptLog(ctx context.Context, orgID string, before time.Time) (int, error)
	// SealPromptLogSegment records seg and deletes the rows it holds in one
	// transaction; if they are not exactly seg.Count rows of seg.OrgID
	// between seg.FirstID and seg.LastID, nothing changes (ErrAuditConflict).
	SealPromptLogSegment(ctx context.Context, seg PromptLogSegment) (PromptLogSegment, error)
	// PromptLogSegments lists segments whose newest row is older than
	// before, oldest first, at most MaxPromptLogSegments.
	PromptLogSegments(ctx context.Context, before time.Time) ([]PromptLogSegment, error)
	DropPromptLogSegment(ctx context.Context, id int64) error
}

var (
	digestRe   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fallRe     = regexp.MustCompile(`^[a-z_]{1,32}$`)
	logModelRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}$`)
)

// PromptLogSettingOf returns the organisation's prompt log setting for a
// member: they may see whether their prompts are kept.
func (s *Service) PromptLogSettingOf(ctx context.Context, p Principal, orgID string) (PromptLogSetting, error) {
	if err := s.Require(ctx, p, orgID, Member); err != nil {
		return PromptLogSetting{}, err
	}
	return s.St.PromptLogSetting(ctx, orgID)
}

// PromptLogEnabled reports whether the organisation keeps a prompt log, for
// the gateway to know whether to hold on to texts. It checks no caller.
func (s *Service) PromptLogEnabled(ctx context.Context, orgID string) (bool, error) {
	set, err := s.St.PromptLogSetting(ctx, orgID)
	return set.Enabled, err
}

// SetPromptLog turns the prompt log on or off and sets its retention. Only
// owners and admins in a signed-in session may; the change is audited first
// and a failed audit refuses it.
func (s *Service) SetPromptLog(ctx context.Context, p Principal, orgID string, set PromptLogSetting) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	if set.RetentionDays < MinPromptLogDays || set.RetentionDays > MaxPromptLogDays {
		return fmt.Errorf("retention_days must be %d to %d: %w", MinPromptLogDays, MaxPromptLogDays, ErrInvalidInput)
	}
	e := actorEvent(p, ActionPromptLogSet, orgID, orgID)
	e.Detail = fmt.Sprintf("enabled=%t retention_days=%d", set.Enabled, set.RetentionDays)
	if _, err := s.Record(ctx, e); err != nil {
		return err
	}
	return s.St.SetPromptLogSetting(ctx, orgID, set)
}

// RecordPromptLog keeps one gateway request when its organisation turned
// the log on, and does nothing otherwise. Texts pass redact.Free and are
// capped; identifiers are checked, never trusted. It checks no caller: the
// gateway calls it for a key it authenticated.
func (s *Service) RecordPromptLog(ctx context.Context, e PromptLogEntry) (stored bool, err error) {
	if e.OrgID == "" {
		return false, fmt.Errorf("org: %w", ErrInvalidInput)
	}
	set, err := s.St.PromptLogSetting(ctx, e.OrgID)
	if err != nil || !set.Enabled {
		return false, err
	}
	e.ID = 0
	if e.At.IsZero() {
		e.At = s.now()
	}
	e.At = e.At.UTC().Truncate(time.Microsecond)
	e.UserID, e.KeyID, e.SessionID, e.Path = clipText(e.UserID, 64), clipText(e.KeyID, 64), clipText(e.SessionID, 128), clipText(e.Path, 64)
	if !logModelRe.MatchString(e.Model) {
		e.Model = "other"
	}
	switch e.Route {
	case "local", "cloud", "rejected":
	default:
		e.Route = "rejected"
	}
	if e.Fallthrough != "" && !fallRe.MatchString(e.Fallthrough) {
		e.Fallthrough = "other"
	}
	if !digestRe.MatchString(e.PromptSHA256) {
		e.PromptSHA256 = ""
	}
	if !digestRe.MatchString(e.ForwardedSHA256) {
		e.ForwardedSHA256 = ""
	}
	e.PromptBytes, e.ForwardedBytes, e.DurationMS, e.Status = max(e.PromptBytes, 0), max(e.ForwardedBytes, 0), max(e.DurationMS, 0), max(e.Status, 0)
	e.Prompt, e.Forwarded = LogText(e.Prompt), LogText(e.Forwarded)
	if e.Route == "local" {
		e.Answer = LogText(e.Answer)
	} else {
		e.Answer = "" // only the local model's answers are kept
	}
	if !e.Shortened {
		e.Forwarded, e.ForwardedSHA256, e.ForwardedBytes = "", "", 0
	}
	if _, err := s.St.AddPromptLog(ctx, e); err != nil {
		return false, fmt.Errorf("prompt log: %w", err)
	}
	return true, nil
}

// LogText is how a text enters the prompt log: cut to PromptLogCapture,
// redacted (secrets and personal data), then capped at MaxPromptLogText, each
// cut on a character boundary.
func LogText(s string) string {
	if s == "" {
		return ""
	}
	return clipUTF8(redact.Free(clipUTF8(strings.ToValidUTF8(s, "�"), PromptLogCapture)), MaxPromptLogText)
}

// clipUTF8 cuts s to at most n bytes without splitting a character.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// PromptLog returns rows of the organisation for an owner or admin in a
// signed-in session, never an API key. The view (action
// gateway.prompt_log.read) or export (gateway.prompt_log.export) is audited
// before any row is read; when the entry cannot be written it answers
// ErrAuditUnavailable and no rows.
func (s *Service) PromptLog(ctx context.Context, p Principal, orgID, action string, f PromptLogFilter) ([]PromptLogEntry, error) {
	if action != ActionPromptLogRead && action != ActionPromptLogExport {
		return nil, fmt.Errorf("action: %w", ErrInvalidInput)
	}
	if err := s.AllowPromptLog(ctx, p, orgID, action, f); err != nil {
		return nil, err
	}
	f.OrgID = orgID
	return s.St.ListPromptLog(ctx, f)
}

// AllowPromptLog checks the caller and records the view or export once; an
// export then pages through ListPromptLog itself.
func (s *Service) AllowPromptLog(ctx context.Context, p Principal, orgID, action string, f PromptLogFilter) error {
	if err := signedIn(p); err != nil {
		return err
	}
	if err := s.Require(ctx, p, orgID, Admin); err != nil {
		return err
	}
	if err := checkPromptLogFilter(f); err != nil {
		return err
	}
	e := actorEvent(p, action, orgID, orgID)
	e.Detail = describePromptLogFilter(f)
	if _, err := s.Record(ctx, e); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditUnavailable, err)
	}
	return nil
}

func checkPromptLogFilter(f PromptLogFilter) error {
	if len(f.UserID) > 64 || len(f.SessionID) > 128 || len(f.Model) > 64 || len(f.Fallthru) > 32 {
		return fmt.Errorf("filter too long: %w", ErrInvalidInput)
	}
	switch f.Route {
	case "", "local", "cloud", "rejected":
	default:
		return fmt.Errorf("route must be local, cloud or rejected: %w", ErrInvalidInput)
	}
	if f.Limit < 0 || f.Limit > MaxPromptLogPage || f.BeforeID < 0 || f.AfterID < 0 {
		return fmt.Errorf("limit or cursor: %w", ErrInvalidInput)
	}
	return nil
}

// describePromptLogFilter names the filter in the audit entry: the bounds
// and identifiers chosen, never row content.
func describePromptLogFilter(f PromptLogFilter) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if !f.Since.IsZero() {
		add("since", f.Since.UTC().Format(time.RFC3339))
	}
	if !f.Until.IsZero() {
		add("until", f.Until.UTC().Format(time.RFC3339))
	}
	add("user", f.UserID)
	add("session", f.SessionID)
	add("model", f.Model)
	add("route", f.Route)
	add("fallthrough", f.Fallthru)
	if f.Shortened != nil {
		add("shortened", fmt.Sprint(*f.Shortened))
	}
	return strings.Join(parts, " ")
}

// MatchPromptLog reports whether e matches f's field filters (stores share it).
func MatchPromptLog(e PromptLogEntry, f PromptLogFilter) bool {
	switch {
	case e.OrgID != f.OrgID,
		!f.Since.IsZero() && e.At.Before(f.Since),
		!f.Until.IsZero() && !e.At.Before(f.Until),
		f.UserID != "" && e.UserID != f.UserID,
		f.SessionID != "" && e.SessionID != f.SessionID,
		f.Model != "" && e.Model != f.Model,
		f.Route != "" && e.Route != f.Route,
		f.Fallthru != "" && e.Fallthrough != f.Fallthru,
		f.Shortened != nil && e.Shortened != *f.Shortened,
		f.BeforeID > 0 && e.ID >= f.BeforeID,
		e.ID <= f.AfterID:
		return false
	}
	return true
}
