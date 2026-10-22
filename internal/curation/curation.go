// Package curation holds what people and agents add to a repository's
// architecture catalog without touching its files: proposals for new records
// and links between records and code. They live next to reviews, apart from
// the code graph, so re-indexing keeps them.
//
// Nothing here approves anything. A proposal is a draft that stays outside the
// repository's own records until someone commits it as a file with an
// accepted status; a link is evidence a person or agent added, shown as such.
// All text is size-limited and redacted on the way in, because it is shown to
// people and fed back to agents.
package curation

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/parisnakitakejser/codesynapse/internal/arch"
	"github.com/parisnakitakejser/codesynapse/internal/redact"
)

// Item kinds.
const (
	KindProposal = "proposal"
	KindLink     = "link"
	KindCode     = "code"
)

// Proposal states. A proposal that a record in the repository now answers is
// shown as merged without changing what is stored.
const (
	Open      = "open"
	Withdrawn = "withdrawn"
)

// Link kinds.
const (
	// Implements says a piece of code carries out a record's decision.
	Implements = "implements"
	// Relates says two records refer to each other.
	Relates = "relates"
	// AppliesTo says a record governs a component.
	AppliesTo = "applies-to"
)

// Limits.
const (
	maxTitle     = 200
	maxBody      = 20000
	maxNote      = 1000
	maxRef       = 300
	maxListItems = 20
	maxName      = 80
	// MaxItemsPerRepo bounds what one repository can accumulate.
	MaxItemsPerRepo = 2000
)

// ErrInvalid wraps validation errors.
var ErrInvalid = errors.New("invalid input")

// Item is one stored proposal or link.
type Item struct {
	ID        string `json:"id"`
	Repo      string `json:"repo"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// OrgID keeps organisations that share one code graph apart.
	OrgID string `json:"org_id,omitempty"`
	// CreatedBy is the user ID (accounts mode) and Client the MCP client or
	// "web", so a person can tell an agent's work from a colleague's.
	CreatedBy string      `json:"created_by,omitempty"`
	Client    string      `json:"client,omitempty"`
	Proposal  *Proposal   `json:"proposal,omitempty"`
	Link      *Link       `json:"link,omitempty"`
	Code      *CodeRecord `json:"code,omitempty"`
}

// Proposal is a draft record. Its status is always the draft word of its type
// (proposed for an ADR, draft for the others): approval happens when a person
// merges a file with a different status, never here.
type Proposal struct {
	Type       string   `json:"type"`
	Title      string   `json:"title"`
	Body       string   `json:"body,omitempty"`
	Status     string   `json:"status"`
	State      string   `json:"state"`
	Topic      string   `json:"topic,omitempty"`
	Components []string `json:"components,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Supersedes []string `json:"supersedes,omitempty"`
	Relates    []string `json:"relates,omitempty"`
	// Implements are code references as written.
	Implements []string `json:"implements,omitempty"`
	// SuggestedID and SuggestedPath say where the file would go.
	SuggestedID   string `json:"suggested_id"`
	SuggestedPath string `json:"suggested_path"`
}

// Link ties a record to code, to another record or to a component.
type Link struct {
	Kind   string `json:"kind"`
	Record string `json:"record"`
	Target string `json:"target"`
	Note   string `json:"note,omitempty"`
}

// ProposalInput is what a caller supplies.
type ProposalInput struct {
	Type       string
	Title      string
	Body       string
	Topic      string
	Components []string
	Labels     []string
	Supersedes []string
	Relates    []string
	Implements []string
}

// NewID returns a random ID with a prefix.
func NewID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:]) //nolint:errcheck // crypto/rand does not fail on supported platforms
	return prefix + "-" + hex.EncodeToString(b[:])
}

// DraftStatus is the word a new proposal of this type starts with.
func DraftStatus(docType string) string {
	if docType == arch.ADR {
		return "proposed"
	}
	return "draft"
}

var proposalTypes = []string{arch.ADR, arch.RFC, arch.Standard, arch.Policy}

var (
	recordIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,99}$`)
	nameRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// PrepareProposal validates and redacts a proposal. now stamps it.
func PrepareProposal(repo, org, user, client string, in ProposalInput, now time.Time) (Item, error) {
	t := strings.ToLower(strings.TrimSpace(in.Type))
	if !slices.Contains(proposalTypes, t) {
		return Item{}, fmt.Errorf("%w: type must be one of %s (an overview, runbook or diagram is written as a file)", ErrInvalid, strings.Join(proposalTypes, ", "))
	}
	title := clean(in.Title, maxTitle)
	if title == "" {
		return Item{}, fmt.Errorf("%w: a title is required", ErrInvalid)
	}
	body := redact.Free(strings.TrimSpace(in.Body))
	if utf8.RuneCountInString(body) > maxBody {
		return Item{}, fmt.Errorf("%w: the body is longer than %d characters", ErrInvalid, maxBody)
	}
	p := &Proposal{Type: t, Title: title, Body: body, Status: DraftStatus(t), State: Open, Topic: strings.ToLower(clean(in.Topic, maxName))}
	var err error
	if p.Components, err = names(in.Components, "components"); err != nil {
		return Item{}, err
	}
	if p.Labels, err = names(in.Labels, "labels"); err != nil {
		return Item{}, err
	}
	for _, list := range []struct {
		name string
		in   []string
		out  *[]string
	}{{"supersedes", in.Supersedes, &p.Supersedes}, {"relates", in.Relates, &p.Relates}} {
		if len(list.in) > maxListItems {
			return Item{}, fmt.Errorf("%w: at most %d entries in %s", ErrInvalid, maxListItems, list.name)
		}
		for _, id := range list.in {
			if id = strings.TrimSpace(id); id != "" {
				if !recordIDRe.MatchString(id) {
					return Item{}, fmt.Errorf("%w: %q is not a record ID", ErrInvalid, id)
				}
				*list.out = append(*list.out, id)
			}
		}
		slices.Sort(*list.out)
		*list.out = slices.Compact(*list.out)
	}
	if len(in.Implements) > maxListItems {
		return Item{}, fmt.Errorf("%w: at most %d code references", ErrInvalid, maxListItems)
	}
	for _, ref := range in.Implements {
		if ref = strings.TrimSpace(ref); ref != "" {
			if utf8.RuneCountInString(ref) > maxRef || strings.ContainsAny(ref, "\n\r") {
				return Item{}, fmt.Errorf("%w: a code reference must be one short line", ErrInvalid)
			}
			p.Implements = append(p.Implements, ref)
		}
	}
	ts := now.UTC().Format(time.RFC3339)
	return Item{ID: NewID("prop"), Repo: repo, Kind: KindProposal, CreatedAt: ts, UpdatedAt: ts, OrgID: org, CreatedBy: user, Client: clean(client, maxName), Proposal: p}, nil
}

// PrepareLink validates and redacts a link.
func PrepareLink(repo, org, user, client, kind, record, target, note string, now time.Time) (Item, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !slices.Contains([]string{Implements, Relates, AppliesTo}, kind) {
		return Item{}, fmt.Errorf("%w: kind must be implements, relates or applies-to", ErrInvalid)
	}
	record, target = strings.TrimSpace(record), strings.TrimSpace(target)
	if !recordIDRe.MatchString(record) {
		return Item{}, fmt.Errorf("%w: record must be a record ID such as ADR-0007", ErrInvalid)
	}
	if target == "" || utf8.RuneCountInString(target) > maxRef || strings.ContainsAny(target, "\n\r") {
		return Item{}, fmt.Errorf("%w: target is required and must be one short line", ErrInvalid)
	}
	if kind == AppliesTo {
		target = strings.ToLower(target)
		if !nameRe.MatchString(target) || len(target) > maxName {
			return Item{}, fmt.Errorf("%w: a component name is lower-case letters, digits, dots, dashes and underscores", ErrInvalid)
		}
	}
	ts := now.UTC().Format(time.RFC3339)
	return Item{ID: NewID("link"), Repo: repo, Kind: KindLink, CreatedAt: ts, UpdatedAt: ts, OrgID: org, CreatedBy: user, Client: clean(client, maxName),
		Link: &Link{Kind: kind, Record: record, Target: target, Note: clean(note, maxNote)}}, nil
}

// clean redacts a one-line text and cuts it to n characters.
func clean(s string, n int) string {
	s = strings.Join(strings.Fields(redact.Free(s)), " ")
	if utf8.RuneCountInString(s) > n {
		return string([]rune(s)[:n])
	}
	return s
}

func names(in []string, what string) ([]string, error) {
	if len(in) > maxListItems {
		return nil, fmt.Errorf("%w: at most %d entries in %s", ErrInvalid, maxListItems, what)
	}
	var out []string
	for _, n := range in {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		if !nameRe.MatchString(n) || len(n) > maxName {
			return nil, fmt.Errorf("%w: %q in %s must be lower-case letters, digits, dots, dashes and underscores", ErrInvalid, n, what)
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}
