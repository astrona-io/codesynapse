package curation

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// CodeInput describes an annotation or an explicitly recorded reuse, split
// or merge. Origins belong to OriginRepo (or the destination repository when
// empty); destinations always belong to the repository storing the record.
type CodeInput struct {
	Method        string   `json:"method"`
	OriginRepo    string   `json:"origin_repo,omitempty"`
	Origins       []string `json:"origins"`
	Destinations  []string `json:"destinations"`
	Purpose       string   `json:"purpose"`
	Labels        []string `json:"labels"`
	Reason        string   `json:"reason"`
	Modifications string   `json:"modifications"`
	Compatibility string   `json:"compatibility"`
	License       string   `json:"license"`
	Attribution   string   `json:"attribution"`
	Validation    string   `json:"validation"`
}

// SymbolSource pins evidence to the revision recorded at creation time.
type SymbolSource struct {
	ID          string `json:"id"`
	Repo        string `json:"repo,omitempty"`
	Revision    string `json:"revision"`
	NodeID      string `json:"node_id"`
	Fingerprint string `json:"fingerprint"`
}

// CodeRecord is immutable evidence. Divergence is computed on reads.
type CodeRecord struct {
	CodeInput
	Sources []SymbolSource `json:"sources"`
	Targets []SymbolSource `json:"targets"`
}

// PrepareCode validates bounded user text. The catalog fills evidence only
// after resolving every reference in the authorised repository.
func PrepareCode(repo, org, user, client string, in CodeInput, now time.Time) (Item, error) {
	in.OriginRepo = strings.TrimSpace(in.OriginRepo)
	if len(in.OriginRepo) > 300 || strings.ContainsAny(in.OriginRepo, "|\\\x00\r\n") {
		return Item{}, fmt.Errorf("%w: invalid origin repository", ErrInvalid)
	}
	if in.Method == "annotation" && in.OriginRepo != "" {
		return Item{}, fmt.Errorf("%w: annotations cannot name an origin repository", ErrInvalid)
	}
	if !slices.Contains([]string{"annotation", "shared-api", "copy", "adapt", "split", "merge"}, in.Method) {
		return Item{}, fmt.Errorf("%w: unsupported reuse method", ErrInvalid)
	}
	if len(in.Destinations) == 0 || len(in.Destinations) > maxListItems || len(in.Origins) > maxListItems {
		return Item{}, fmt.Errorf("%w: one to %d destinations and at most %d origins required", ErrInvalid, maxListItems, maxListItems)
	}
	if in.Method == "annotation" && len(in.Origins) != 0 || in.Method != "annotation" && len(in.Origins) == 0 {
		return Item{}, fmt.Errorf("%w: reuse needs origins; annotations have none", ErrInvalid)
	}
	if in.Method == "split" && (len(in.Origins) != 1 || len(in.Destinations) < 2) || in.Method == "merge" && (len(in.Origins) < 2 || len(in.Destinations) != 1) {
		return Item{}, fmt.Errorf("%w: split needs one origin and several targets; merge needs several origins and one target", ErrInvalid)
	}
	for _, ids := range [][]string{in.Origins, in.Destinations} {
		seen := map[string]bool{}
		for _, id := range ids {
			if !strings.HasPrefix(id, "sym-") || len(id) != 36 || seen[id] {
				return Item{}, fmt.Errorf("%w: unique permanent symbol IDs required", ErrInvalid)
			}
			if _, err := hex.DecodeString(id[4:]); err != nil {
				return Item{}, fmt.Errorf("%w: invalid permanent symbol ID", ErrInvalid)
			}
			seen[id] = true
		}
	}
	for _, id := range in.Origins {
		if (in.OriginRepo == "" || in.OriginRepo == repo) && slices.Contains(in.Destinations, id) {
			return Item{}, fmt.Errorf("%w: origin and destination must differ", ErrInvalid)
		}
	}
	for _, field := range []*string{&in.Purpose, &in.Reason, &in.Modifications, &in.Compatibility, &in.License, &in.Attribution, &in.Validation} {
		if utf8.RuneCountInString(*field) > maxNote {
			return Item{}, fmt.Errorf("%w: code record text exceeds %d characters", ErrInvalid, maxNote)
		}
		*field = clean(*field, maxNote)
	}
	if in.Method == "annotation" && in.Purpose == "" || in.Method != "annotation" && (in.Reason == "" || in.Compatibility == "" || in.License == "" || in.Attribution == "" || in.Validation == "") {
		return Item{}, fmt.Errorf("%w: annotations need purpose; reuse needs reason, compatibility, license, attribution and validation (state unknown or not run explicitly)", ErrInvalid)
	}
	var err error
	in.Labels, err = names(in.Labels, "labels")
	if err != nil {
		return Item{}, err
	}
	if in.Labels == nil {
		in.Labels = []string{}
	}
	in.Origins = slices.Clone(in.Origins)
	in.Destinations = slices.Clone(in.Destinations)
	stamp := now.UTC().Format(time.RFC3339Nano)
	return Item{ID: NewID("code"), Repo: repo, Kind: KindCode, OrgID: org, CreatedBy: user, Client: clean(client, maxName), CreatedAt: stamp, UpdatedAt: stamp, Code: &CodeRecord{CodeInput: in, Sources: []SymbolSource{}, Targets: []SymbolSource{}}}, nil
}
