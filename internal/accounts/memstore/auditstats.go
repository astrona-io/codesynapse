package memstore

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// AuditStats implements accounts.Store.
func (s *Store) AuditStats(_ context.Context, since, until time.Time) (accounts.AuditCounts, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type hourKey struct {
		hour            time.Time
		family, outcome string
	}
	hours := map[hourKey]int{}
	logins, security := map[string]int{}, map[string]int{}
	actions := accounts.AuditSecurityActionList()
	for _, e := range s.events {
		if e.At.Before(since) || !e.At.Before(until) {
			continue
		}
		family := e.Action
		if strings.HasPrefix(family, "http ") {
			family = "http"
		} else if i := strings.IndexByte(family, '.'); i >= 0 {
			family = family[:i]
		}
		hours[hourKey{e.At.UTC().Truncate(time.Hour), family, e.Outcome}]++
		if e.Action == "user.login" && e.Outcome == accounts.Failure {
			logins[e.Detail]++
		}
		if slices.Contains(actions, e.Action) {
			security[e.Action]++
		}
	}
	out := accounts.AuditCounts{Hours: []accounts.AuditHourCount{}, Logins: named(logins), Security: named(security)}
	for k, n := range hours {
		out.Hours = append(out.Hours, accounts.AuditHourCount{Hour: k.hour, Family: k.family, Outcome: k.outcome, N: n})
	}
	slices.SortFunc(out.Hours, func(a, b accounts.AuditHourCount) int {
		return cmp.Or(a.Hour.Compare(b.Hour), strings.Compare(a.Family, b.Family), strings.Compare(a.Outcome, b.Outcome))
	})
	out.Hours = out.Hours[:min(len(out.Hours), accounts.MaxAuditCountRows)]
	return out, nil
}

func named(m map[string]int) []accounts.AuditNamedCount {
	out := []accounts.AuditNamedCount{}
	for k, n := range m {
		out = append(out, accounts.AuditNamedCount{Name: k, N: n})
	}
	slices.SortFunc(out, func(a, b accounts.AuditNamedCount) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// AddAuditVerification implements accounts.Store.
func (s *Store) AddAuditVerification(_ context.Context, v accounts.AuditVerification) (accounts.AuditVerification, error) {
	v, err := accounts.PrepareAuditVerification(v)
	if err != nil {
		return v, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifyID++
	v.ID = s.verifyID
	cutoff := v.At.Add(-accounts.AuditVerificationRetention)
	s.verifications = slices.DeleteFunc(s.verifications, func(x accounts.AuditVerification) bool { return x.At.Before(cutoff) })
	s.verifications = append(s.verifications, v)
	return v, nil
}

// AuditVerifications implements accounts.Store.
func (s *Store) AuditVerifications(_ context.Context, limit int) ([]accounts.AuditVerification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := slices.Clone(s.verifications)
	slices.SortStableFunc(out, func(a, b accounts.AuditVerification) int { return cmp.Or(b.At.Compare(a.At), cmp.Compare(b.ID, a.ID)) })
	return out[:min(len(out), min(max(limit, 1), accounts.MaxAuditVerifications))], nil
}

// TriageCounts implements accounts.Store.
func (s *Store) TriageCounts(_ context.Context, now time.Time) (map[string]map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]map[string]int{}
	for _, entry := range s.triage {
		for _, t := range entry {
			if !t.Active(now) {
				continue
			}
			if out[t.OrgID] == nil {
				out[t.OrgID] = map[string]int{}
			}
			out[t.OrgID][t.State]++
		}
	}
	return out, nil
}
