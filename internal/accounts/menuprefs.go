package accounts

import (
	"context"
	"fmt"
	"regexp"
)

// A person's choices for the repository menu, kept so they follow them to
// another browser. Pages are named by the last segment of their path
// ("licenses", "security"); nothing else is stored.

// MaxMenuEntries bounds each list.
const MaxMenuEntries = 40

var menuSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// MenuPrefs are the pages a person pinned to the top of the repository menu
// and the ones they hid.
type MenuPrefs struct {
	Pinned []string `json:"pinned"`
	Hidden []string `json:"hidden"`
}

// Normalise validates the lists and returns them without duplicates, in the
// order given. A page cannot be both pinned and hidden: pinning wins.
func (m MenuPrefs) Normalise() (MenuPrefs, error) {
	clean := func(in []string, name string) ([]string, error) {
		if len(in) > MaxMenuEntries {
			return nil, fmt.Errorf("%w: at most %d %s pages", ErrInvalidInput, MaxMenuEntries, name)
		}
		out := make([]string, 0, len(in))
		seen := map[string]bool{}
		for _, s := range in {
			if !menuSlug.MatchString(s) {
				return nil, fmt.Errorf("%w: %q is not a page name", ErrInvalidInput, s)
			}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		return out, nil
	}
	pinned, err := clean(m.Pinned, "pinned")
	if err != nil {
		return MenuPrefs{}, err
	}
	hidden, err := clean(m.Hidden, "hidden")
	if err != nil {
		return MenuPrefs{}, err
	}
	pin := map[string]bool{}
	for _, s := range pinned {
		pin[s] = true
	}
	kept := hidden[:0]
	for _, s := range hidden {
		if !pin[s] {
			kept = append(kept, s)
		}
	}
	return MenuPrefs{Pinned: pinned, Hidden: kept}, nil
}

// MenuPrefs returns the caller's menu choices; none yet is an empty value.
func (s *Service) MenuPrefs(ctx context.Context, p Principal) (MenuPrefs, error) {
	if err := signedIn(p); err != nil {
		return MenuPrefs{}, err
	}
	return s.St.MenuPrefs(ctx, p.User.ID)
}

// SetMenuPrefs replaces the caller's menu choices.
func (s *Service) SetMenuPrefs(ctx context.Context, p Principal, m MenuPrefs) (MenuPrefs, error) {
	if err := signedIn(p); err != nil {
		return MenuPrefs{}, err
	}
	m, err := m.Normalise()
	if err != nil {
		return MenuPrefs{}, err
	}
	if err := s.St.SetMenuPrefs(ctx, p.User.ID, m); err != nil {
		return MenuPrefs{}, err
	}
	return m, nil
}
