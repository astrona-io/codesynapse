package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// The audit section of the platform statistics (docs/plans/PLATFORM_STATS_PLAN.md,
// phase 7): entries by action family and outcome, failed sign-ins by cause,
// the security events, the stored chain verification and the archive. Up
// to 24 hours is counted live (hourly, on the (at, action) index); longer
// ranges read the daily rollup (auditStatSource) plus today counted live.
// Counts and fixed names only: no actor, target, address or detail text.
// The chain status is the stored result of the last verification
// (verifyAuditChain, or a platform admin's on-demand run); a view never
// walks the chain.

// AuditBucket is the number of entries in one hour or day.
type AuditBucket struct {
	At      time.Time `json:"at"`
	Success int       `json:"success"`
	Failure int       `json:"failure"`
}

// AuditFamilyCount is one action family's entries in the range.
type AuditFamilyCount struct {
	Name    string `json:"name"`
	Success int    `json:"success"`
	Failure int    `json:"failure"`
}

// AuditArchiveStats describes the sealed part of the log.
type AuditArchiveStats struct {
	Configured bool       `json:"configured"`
	Segments   int        `json:"segments"`
	Sealed     int        `json:"sealed"`
	LastSeal   *time.Time `json:"last_seal"`
	OldestHot  *time.Time `json:"oldest_hot"`
}

// AuditSection is the audit section.
type AuditSection struct {
	Range string `json:"range"`
	// Step is "hour" (24h and shorter) or "day".
	Step     string                     `json:"step"`
	Buckets  []AuditBucket              `json:"buckets"`
	Families []AuditFamilyCount         `json:"families"`
	Logins   []accounts.AuditNamedCount `json:"logins"`
	Security []accounts.AuditNamedCount `json:"security"`
	// Chain is the latest stored verification (null before the first);
	// Verifications the recent ones, newest first.
	Chain         *accounts.AuditVerification  `json:"chain"`
	Verifications []accounts.AuditVerification `json:"verifications"`
	Archive       AuditArchiveStats            `json:"archive"`
	// Rollup is set for ranges longer than 24 hours: the days the rollup
	// held (today is always counted live).
	Rollup *StatsRollup `json:"rollup,omitempty"`
}

// auditRecentVerifications is how many stored results the section lists.
const auditRecentVerifications = 30

// auditLiveFallbackDays: a day of a range the rollup has not written is
// counted live when it is this recent (the events are still in PostgreSQL
// and the index keeps it cheap); older missing days are reported as not
// written.
const auditLiveFallbackDays = 30

// auditMetrics are the rollup metrics of the section.
var auditMetrics = []string{accounts.StatAuditEvents, accounts.StatAuditLogins, accounts.StatAuditSecurity}

func (s *Server) handleStatsAudit(w http.ResponseWriter, r *http.Request) {
	req := statsRequest(r, "audit")
	if req.OrgID != "" {
		s.problem(w, http.StatusBadRequest, errors.New("the audit section is platform-wide; org is not accepted"))
		return
	}
	q, ok := s.platformView(w, r, req)
	if !ok {
		return
	}
	out, err := s.auditStats(r.Context(), req.Range, q)
	if err != nil {
		s.statsError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// auditTally collects counts before they become the section.
type auditTally struct {
	buckets  map[time.Time]*AuditBucket
	families map[string]*AuditFamilyCount
	logins   map[string]int
	security map[string]int
}

func newAuditTally() *auditTally {
	return &auditTally{buckets: map[time.Time]*AuditBucket{}, families: map[string]*AuditFamilyCount{}, logins: map[string]int{}, security: map[string]int{}}
}

func (t *auditTally) add(at time.Time, family, outcome string, n int) {
	if n <= 0 || (outcome != accounts.Success && outcome != accounts.Failure) {
		return
	}
	b := t.buckets[at]
	if b == nil {
		b = &AuditBucket{At: at}
		t.buckets[at] = b
	}
	f := t.families[family]
	if f == nil {
		f = &AuditFamilyCount{Name: family}
		t.families[family] = f
	}
	if outcome == accounts.Success {
		b.Success += n
		f.Success += n
	} else {
		b.Failure += n
		f.Failure += n
	}
}

// addLive adds a live count; day groups its hours into days.
func (t *auditTally) addLive(c accounts.AuditCounts, day bool) {
	for _, h := range c.Hours {
		at := h.Hour
		if day {
			at = accounts.StatDay(at)
		}
		t.add(at, accounts.AuditFamily(h.Family), h.Outcome, h.N)
	}
	for _, l := range c.Logins {
		t.logins[accounts.LoginCause(l.Name)] += l.N
	}
	for _, sc := range c.Security {
		if name, ok := accounts.AuditSecurityActions[sc.Name]; ok {
			t.security[name] += sc.N
		}
	}
}

// addRollup adds a rollup row.
func (t *auditTally) addRollup(r accounts.PlatformStat) {
	switch r.Metric {
	case accounts.StatAuditEvents:
		for _, outcome := range []string{accounts.Success, accounts.Failure} {
			if fam, ok := cutSuffix(r.Dimension, "."+outcome); ok {
				t.add(r.Day, fam, outcome, int(r.Value))
			}
		}
	case accounts.StatAuditLogins:
		t.logins[r.Dimension] += int(r.Value)
	case accounts.StatAuditSecurity:
		t.security[r.Dimension] += int(r.Value)
	}
}

func cutSuffix(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)], true
	}
	return "", false
}

// named lists fixed names in their order, with their counts (zeros too).
func namedCounts(names []string, m map[string]int) []accounts.AuditNamedCount {
	out := make([]accounts.AuditNamedCount, 0, len(names))
	for _, n := range names {
		out = append(out, accounts.AuditNamedCount{Name: n, N: m[n]})
	}
	return out
}

func (s *Server) auditStats(ctx context.Context, rng string, q accounts.StatsQuery) (AuditSection, error) {
	st := s.cfg.Accounts.St
	out := AuditSection{Range: rng, Step: "hour"}
	t := newAuditTally()
	if q.Daily == "" && q.Now.Sub(q.Since) <= 24*time.Hour {
		c, err := st.AuditStats(ctx, q.Since, q.Now)
		if err != nil {
			return out, err
		}
		t.addLive(c, false)
	} else {
		out.Step = "day"
		from, today := accounts.StatDay(q.Since), accounts.StatDay(q.Now)
		rollup := &StatsRollup{Days: int(today.Sub(from) / (24 * time.Hour)), Unavailable: []string{}}
		written := map[time.Time]bool{}
		if today.After(from) {
			rows, err := st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: from, To: today, Metrics: auditMetrics})
			if err != nil {
				return out, err
			}
			if len(rows) >= accounts.MaxPlatformStatRows {
				return out, errDailyTooLarge
			}
			for _, r := range rows {
				written[r.Day] = true
				t.addRollup(r)
			}
		}
		rollup.Written = len(written)
		for day := range written {
			if rollup.Latest == nil || day.After(*rollup.Latest) {
				d := day
				rollup.Latest = &d
			}
		}
		for day := from; day.Before(today); day = day.AddDate(0, 0, 1) {
			if written[day] || today.Sub(day) > auditLiveFallbackDays*24*time.Hour {
				continue
			}
			c, err := st.AuditStats(ctx, day, day.AddDate(0, 0, 1))
			if err != nil {
				return out, err
			}
			t.addLive(c, true)
		}
		c, err := st.AuditStats(ctx, today, q.Now)
		if err != nil {
			return out, err
		}
		t.addLive(c, true)
		out.Rollup = rollup
	}
	out.Buckets = make([]AuditBucket, 0, len(t.buckets))
	for _, b := range t.buckets {
		out.Buckets = append(out.Buckets, *b)
	}
	slices.SortFunc(out.Buckets, func(a, b AuditBucket) int { return a.At.Compare(b.At) })
	out.Families = []AuditFamilyCount{}
	for _, name := range append(slices.Clone(accounts.AuditFamilies), accounts.AuditFamilyOther) {
		if f := t.families[name]; f != nil {
			out.Families = append(out.Families, *f)
		}
	}
	out.Logins = namedCounts(accounts.LoginCauseNames, t.logins)
	out.Security = namedCounts(accounts.AuditSecurityNames, t.security)
	var err error
	if out.Verifications, err = st.AuditVerifications(ctx, auditRecentVerifications); err != nil {
		return out, err
	}
	if len(out.Verifications) > 0 {
		latest := out.Verifications[0]
		out.Chain = &latest
	}
	out.Archive, err = s.auditArchiveStats(ctx, q.Now)
	return out, err
}

// auditArchiveStats reads the segments sealed by until and the oldest entry
// still in PostgreSQL.
func (s *Server) auditArchiveStats(ctx context.Context, until time.Time) (AuditArchiveStats, error) {
	st := s.cfg.Accounts.St
	a := AuditArchiveStats{Configured: s.cfg.AuditArchive != nil}
	segs, err := st.AuditSegments(ctx)
	if err != nil {
		return a, err
	}
	for _, sg := range segs {
		if sg.SealedAt.After(until) {
			continue
		}
		a.Segments++
		a.Sealed += sg.Count
		if a.LastSeal == nil || sg.SealedAt.After(*a.LastSeal) {
			at := sg.SealedAt.UTC()
			a.LastSeal = &at
		}
	}
	oldest, err := st.ListEvents(ctx, accounts.EventFilter{Limit: 1, Ascending: true})
	if err != nil {
		return a, err
	}
	if len(oldest) > 0 {
		at := oldest[0].At.UTC()
		a.OldestHot = &at
	}
	return a, nil
}

// auditExport is the audit section for export: one row per bucket and
// family, then the named counts.
func auditExport(a AuditSection) (body any, header []string, rows [][]string) {
	header = []string{"kind", "name", "success", "failure"}
	for _, b := range a.Buckets {
		rows = append(rows, []string{a.Step, b.At.UTC().Format(time.RFC3339), strconv.Itoa(b.Success), strconv.Itoa(b.Failure)})
	}
	for _, f := range a.Families {
		rows = append(rows, []string{"family", f.Name, strconv.Itoa(f.Success), strconv.Itoa(f.Failure)})
	}
	for _, l := range a.Logins {
		rows = append(rows, []string{"failed_sign_in", l.Name, "", strconv.Itoa(l.N)})
	}
	for _, sc := range a.Security {
		rows = append(rows, []string{"security", sc.Name, strconv.Itoa(sc.N), ""})
	}
	if c := a.Chain; c != nil {
		state := "verified"
		switch {
		case c.RunError:
			state = "run_error"
		case !c.OK:
			state = "broken"
		}
		rows = append(rows, []string{"chain", state, c.At.UTC().Format(time.RFC3339), ""})
	}
	return a, header, rows
}

// auditStatSource adds a day's audit counts to the daily rollup, and the
// chain verifications of the day and the archive at its end.
func (s *Server) auditStatSource(ctx context.Context, day time.Time, set *accounts.PlatformStatSet) error {
	st := s.cfg.Accounts.St
	end := day.AddDate(0, 0, 1)
	c, err := st.AuditStats(ctx, day, end)
	if err != nil {
		return err
	}
	for _, h := range c.Hours {
		if h.Outcome == accounts.Success || h.Outcome == accounts.Failure {
			set.Total(accounts.StatAuditEvents, accounts.AuditFamily(h.Family)+"."+h.Outcome, int64(h.N))
		}
	}
	for _, l := range c.Logins {
		set.Total(accounts.StatAuditLogins, accounts.LoginCause(l.Name), int64(l.N))
	}
	for _, sc := range c.Security {
		if name, ok := accounts.AuditSecurityActions[sc.Name]; ok {
			set.Total(accounts.StatAuditSecurity, name, int64(sc.N))
		}
	}
	vs, err := st.AuditVerifications(ctx, accounts.MaxAuditVerifications)
	if err != nil {
		return err
	}
	for _, v := range vs {
		if v.At.Before(day) || !v.At.Before(end) {
			continue
		}
		switch {
		case v.RunError:
			set.Total(accounts.StatAuditChain, "run_error", 1)
		case v.OK:
			set.Total(accounts.StatAuditChain, "verified", 1)
		default:
			set.Total(accounts.StatAuditChain, "broken", 1)
		}
	}
	a, err := s.auditArchiveStats(ctx, end)
	if err != nil {
		return err
	}
	set.Total(accounts.StatAuditChain, "segments", int64(a.Segments))
	set.Total(accounts.StatAuditChain, "sealed", int64(a.Sealed))
	return nil
}

// auditVerifyEvery is how often the chain is verified on schedule; a run
// on demand counts, so a platform admin's check moves the next one.
const auditVerifyEvery = 24 * time.Hour

// auditVerifyRetry is the shortest wait between runs, and the wait after a
// store error.
const auditVerifyRetry = 10 * time.Minute

// verifyAuditChain verifies the audit chain once a day and stores the
// result (docs/plans/PLATFORM_STATS_PLAN.md, phase 7). It runs under the
// api-loops lease, so one process verifies; the statistics read the stored
// result instead of walking the chain on every view.
func (s *Server) verifyAuditChain(ctx context.Context) {
	for {
		wait := s.verifyAuditChainOnce(ctx, time.Now())
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// verifyAuditChainOnce runs a verification when the last stored one is a
// day old or missing, and answers the wait until the next is due.
func (s *Server) verifyAuditChainOnce(ctx context.Context, now time.Time) time.Duration {
	latest, err := s.cfg.Accounts.LatestAuditVerification(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("reading the last audit chain verification failed", "error", err)
		}
		return auditVerifyRetry
	}
	if latest == nil || now.Sub(latest.At) >= auditVerifyEvery {
		v, err := s.runAuditVerification(ctx, accounts.VerifyScheduled, false)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Warn("the scheduled audit chain verification failed", "error", err)
			}
			return auditVerifyRetry
		}
		latest = &v
	}
	return max(latest.At.Add(auditVerifyEvery).Sub(now), auditVerifyRetry)
}

// runAuditVerification verifies the chain (deep: archive files too) and
// stores the result. A broken chain or a run that could not finish is also
// written to the audit log and the service log.
func (s *Server) runAuditVerification(ctx context.Context, trigger string, deep bool) (accounts.AuditVerification, error) {
	verify := func(ctx context.Context) (accounts.Verification, error) {
		return accounts.VerifyAudit(ctx, s.cfg.Accounts.St)
	}
	if deep && s.cfg.AuditArchive != nil {
		verify = func(ctx context.Context) (accounts.Verification, error) {
			return deepVerifyAudit(ctx, s.cfg.Accounts.St, s.cfg.AuditArchive)
		}
	}
	v, err := s.cfg.Accounts.RunAuditVerification(ctx, trigger, deep, verify)
	if err == nil && !v.OK {
		s.logger.Error("the audit chain did not verify", "broken_at", v.BrokenAt, "reason", v.Reason, "trigger", trigger)
		s.record(ctx, accounts.Event{Via: "service", Action: "audit.verify", Outcome: accounts.Failure,
			Detail: "chain broken at entry " + strconv.FormatInt(v.BrokenAt, 10) + ": " + v.Reason})
	}
	return v, err
}
