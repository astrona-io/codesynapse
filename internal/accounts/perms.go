package accounts

import (
	"encoding/json"
	"fmt"
	"math/bits"
)

// Perms is a set of permissions. Built-in roles, custom roles and key
// scopes all reduce to one, so a check asks the set rather than the role
// that produced it (docs/plans/OPENFGA_PLAN.md, "Custom roles").
type Perms uint64

// permBit numbers every known permission in sorted order; permAt is the
// reverse. Both are fixed at start-up.
var (
	permAt  = AllPermissions()
	permBit = func() map[Permission]uint {
		if len(permAt) > 64 {
			panic("accounts: more than 64 permissions do not fit in Perms")
		}
		m := make(map[Permission]uint, len(permAt))
		for i, p := range permAt {
			m[p] = uint(i)
		}
		return m
	}()
)

var levelRank = map[string]int{LevelRead: 1, LevelWrite: 2, LevelFull: 3}

// PermsOf is the set holding ps and, for each, the lower levels of the same
// area (full includes write and read). Unknown permissions are ignored.
func PermsOf(ps ...Permission) Perms {
	var out Perms
	for _, p := range ps {
		if _, ok := permBit[p]; !ok {
			continue
		}
		out |= 1 << permBit[p]
		if p.Marker() {
			continue
		}
		for _, q := range permAt {
			if !q.Marker() && q.Resource() == p.Resource() && q.Area() == p.Area() && levelRank[q.Level()] <= levelRank[p.Level()] {
				out |= 1 << permBit[q]
			}
		}
	}
	return out
}

// Has reports whether p is in the set.
func (s Perms) Has(p Permission) bool {
	b, ok := permBit[p]
	return ok && s&(1<<b) != 0
}

// Union is every permission in s or o.
func (s Perms) Union(o Perms) Perms { return s | o }

// Intersect is every permission in both s and o.
func (s Perms) Intersect(o Perms) Perms { return s & o }

// Covers reports whether s holds every permission of o.
func (s Perms) Covers(o Perms) bool { return o&^s == 0 }

// Empty reports whether s holds nothing.
func (s Perms) Empty() bool { return s == 0 }

// Len is the number of permissions in s.
func (s Perms) Len() int { return bits.OnesCount64(uint64(s)) }

// Only keeps the permissions of one resource.
func (s Perms) Only(resource string) Perms {
	var out Perms
	for _, p := range s.List() {
		if p.Resource() == resource {
			out |= 1 << permBit[p]
		}
	}
	return out
}

// List is the set as sorted permissions.
func (s Perms) List() []Permission {
	out := []Permission{}
	for i, p := range permAt {
		if s&(1<<uint(i)) != 0 {
			out = append(out, p)
		}
	}
	return out
}

// MarshalJSON writes the set as a sorted list of names.
func (s Perms) MarshalJSON() ([]byte, error) { return json.Marshal(s.List()) }

// UnmarshalJSON reads a list of names; an unknown name is an error.
func (s *Perms) UnmarshalJSON(b []byte) error {
	var ps []Permission
	if err := json.Unmarshal(b, &ps); err != nil {
		return err
	}
	var out Perms
	for _, p := range ps {
		if !p.Valid() {
			return fmt.Errorf("unknown permission %q", p)
		}
		out |= PermsOf(p)
	}
	*s = out
	return nil
}

// Bundle is what the built-in role r holds on resource.
func (r Role) Bundle(resource string) Perms { return PermsOf(r.Permissions(resource)...) }

// AllPerms holds every known permission.
func AllPerms() Perms { return PermsOf(permAt...) }

// BuiltinRepoRole is the strongest built-in repository role whose bundle s
// covers ("" when s does not even cover read). Job records and other
// role-based callers use it for a custom grant.
func BuiltinRepoRole(s Perms) Role {
	for _, r := range []Role{Full, Write, Read} {
		if s.Covers(r.Bundle(ResourceRepository)) {
			return r
		}
	}
	return ""
}
