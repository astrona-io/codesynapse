package curation

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// IdentityReviewInput is a person's revision-pinned identity assertion. It
// creates an organisation-local link, never changes shared graph identities.
type IdentityReviewInput struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Relation  string `json:"relation"`
	Reason    string `json:"reason"`
	Revision  string `json:"revision"`
	IndexedAt string `json:"indexed_at"`
}

// IdentityReview retains original evidence and the reviews this one replaces.
// Superseded reviews remain stored. Competing heads fail closed on reads.
type IdentityReview struct {
	IdentityReviewInput
	Source     SymbolSource `json:"source"`
	Target     SymbolSource `json:"target"`
	Supersedes []string     `json:"supersedes"`
	SourceFile string       `json:"source_file,omitempty"`
	TargetFile string       `json:"target_file,omitempty"`
}

func validIdentityID(id string) bool {
	if len(id) != 36 || !strings.HasPrefix(id, "sym-") {
		return false
	}
	_, err := hex.DecodeString(id[4:])
	return err == nil
}

// PrepareIdentityReview validates bounded, redacted human review input.
func PrepareIdentityReview(repo, org, user string, in IdentityReviewInput, now time.Time) (Item, error) {
	if !validIdentityID(in.From) || !validIdentityID(in.To) || in.From == in.To {
		return Item{}, fmt.Errorf("%w: two different permanent symbol IDs are required", ErrInvalid)
	}
	if !slices.Contains([]string{"same-symbol", "copy", "distinct"}, in.Relation) {
		return Item{}, fmt.Errorf("%w: relation must be same-symbol, copy or distinct", ErrInvalid)
	}
	if len(in.Revision) > 200 || len(in.IndexedAt) > 100 || in.Revision == "" && in.IndexedAt == "" || utf8.RuneCountInString(in.Reason) > maxNote {
		return Item{}, fmt.Errorf("%w: bounded revision evidence and reason required", ErrInvalid)
	}
	in.Reason = clean(in.Reason, maxNote)
	if in.Reason == "" {
		return Item{}, fmt.Errorf("%w: review reason is required", ErrInvalid)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	return Item{ID: NewID("identity"), Repo: repo, Kind: KindIdentityReview, OrgID: org, CreatedBy: user, Client: "web", CreatedAt: stamp, UpdatedAt: stamp, IdentityReview: &IdentityReview{IdentityReviewInput: in, Supersedes: []string{}}}, nil
}
