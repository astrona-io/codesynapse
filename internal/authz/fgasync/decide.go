package fgasync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
)

// Checker is the part of fga.Client a Decider asks.
type Checker interface {
	BatchCheck(ctx context.Context, checks []fga.Tuple) ([]bool, error)
	ListObjects(ctx context.Context, user, relation, objectType string) ([]string, error)
	StoreID() string
	ModelID() string
}

// Decider bounds.
const (
	// maxBatchChecks is the most checks one decision sends as BatchCheck
	// calls; above it, repository permissions are listed per relation.
	maxBatchChecks = 8 * fga.MaxBatchCheck
	// maxFallbackRepos is the most repositories one relation is checked
	// for one by one when its ListObjects answer was cut.
	maxFallbackRepos = 1000
	// parallel bounds the OpenFGA calls one decision runs at once.
	parallel = 4
	// selectEvery is how long a selected store and model are used before
	// they are looked up again; selectRetry the wait after a failed look-up.
	selectEvery = 30 * time.Second
	selectRetry = 5 * time.Second
)

// Decider answers accounts.Decider from OpenFGA (docs/plans/OPENFGA_PLAN.md,
// phase 6). It only reads: Select finds the store and model the sync wrote
// and never creates one. An organisation whose sync record is behind its
// policy revision, or names another store or model, is left out of the
// answer (stale); any error, including a truncated list that cannot be
// checked one by one, refuses the whole decision.
type Decider struct {
	Records accounts.OpenFGASyncStore
	// Select finds the store and model to ask (fga.Client.Select).
	Select func(ctx context.Context) (Checker, error)
	Now    func() time.Time

	mu       sync.Mutex
	cur      Checker
	selected time.Time
	failed   time.Time
}

func (d *Decider) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// checker returns the selected client, looking it up again every
// selectEvery. A failed look-up keeps the previous one: Fresh still
// compares its store and model with each organisation's sync record.
func (d *Decider) checker(ctx context.Context) (Checker, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if d.cur != nil && now.Sub(d.selected) < selectEvery {
		return d.cur, nil
	}
	if !d.failed.IsZero() && now.Sub(d.failed) < selectRetry {
		if d.cur != nil {
			return d.cur, nil
		}
		return nil, fga.ErrNoStore
	}
	c, err := d.Select(ctx)
	if err != nil || c == nil || c.StoreID() == "" || c.ModelID() == "" {
		d.failed = now
		if d.cur != nil {
			return d.cur, nil
		}
		if err == nil {
			err = fga.ErrNoStore
		}
		return nil, fmt.Errorf("openfga: %w", err)
	}
	d.cur, d.selected, d.failed = c, now, time.Time{}
	return c, nil
}

// Decide implements accounts.Decider.
func (d *Decider) Decide(ctx context.Context, q accounts.DecisionRequest) (accounts.Decision, error) {
	out := accounts.Decision{Orgs: map[string]accounts.OrgDecision{}, Repos: map[string]accounts.Perms{}}
	if len(q.Orgs) == 0 {
		return out, nil
	}
	user, err := object(fga.TypeUser, q.UserID)
	if err != nil {
		return accounts.Decision{}, err
	}
	ck, err := d.checker(ctx)
	if err != nil {
		return accounts.Decision{}, err
	}
	fresh := map[string]bool{}
	var orgObjects []string
	for _, o := range q.Orgs {
		err := Fresh(ctx, d.Records, ck.StoreID(), ck.ModelID(), o)
		if errors.Is(err, ErrStale) {
			continue
		}
		if err != nil {
			return accounts.Decision{}, err
		}
		obj, err := object(fga.TypeOrganisation, o.ID)
		if err != nil {
			return accounts.Decision{}, err
		}
		fresh[o.ID] = true
		orgObjects = append(orgObjects, obj)
	}
	orgRels := orgRelations()
	orgHeld, err := batch(ctx, ck, user, orgRels, orgObjects)
	if err != nil {
		return accounts.Decision{}, err
	}
	for _, o := range q.Orgs {
		if !fresh[o.ID] {
			continue
		}
		held := orgHeld[fga.TypeOrganisation+":"+o.ID]
		od := accounts.OrgDecision{Active: held["active"]}
		if od.Active {
			od.Role = accounts.Guest
			for _, r := range []accounts.Role{accounts.Owner, accounts.Full, accounts.Write, accounts.Read} {
				if held[orgLevel[r]] {
					od.Role = r
					break
				}
			}
			od.Perms = permsOf(held, accounts.ResourceOrganisation)
		}
		out.Orgs[o.ID] = od
	}

	keys := map[string]string{} // repository object -> RepoObject
	var repoObjects []string
	for _, r := range q.Repos {
		if !fresh[r.OrgID] || !out.Orgs[r.OrgID].Active {
			continue
		}
		obj := fga.TypeRepository + ":" + fga.RepositoryID(r.OrgID, r.Graph)
		if _, dup := keys[obj]; dup {
			continue
		}
		keys[obj] = accounts.RepoObject(r.OrgID, r.Graph)
		repoObjects = append(repoObjects, obj)
	}
	if len(repoObjects) == 0 {
		return out, nil
	}
	// Code read first: the other permissions are asked only where it holds.
	codeRead := Relation(accounts.RepoCodeRead)
	reach, err := resolve(ctx, ck, user, []string{codeRead}, repoObjects)
	if err != nil {
		return accounts.Decision{}, err
	}
	var reachable []string
	for _, obj := range repoObjects {
		if reach[obj][codeRead] {
			reachable = append(reachable, obj)
		}
	}
	held, err := resolve(ctx, ck, user, repoRelations(codeRead), reachable)
	if err != nil {
		return accounts.Decision{}, err
	}
	for _, obj := range reachable {
		h := held[obj]
		if h == nil {
			h = map[string]bool{}
		}
		h[codeRead] = true
		out.Repos[keys[obj]] = permsOf(h, accounts.ResourceRepository)
	}
	return out, nil
}

// orgRelations are the organisation relations a decision asks: the levels
// and every organisation permission (the markers are active and owner).
func orgRelations() []string {
	rels := []string{orgLevel[accounts.Read], orgLevel[accounts.Write], orgLevel[accounts.Full]}
	for _, p := range accounts.AllPermissions() {
		if p.Resource() == accounts.ResourceOrganisation {
			rels = append(rels, Relation(p))
		}
	}
	return rels
}

// repoRelations are the repository permission relations except skip.
func repoRelations(skip string) []string {
	var rels []string
	for _, p := range accounts.AllPermissions() {
		if p.Resource() == accounts.ResourceRepository && Relation(p) != skip {
			rels = append(rels, Relation(p))
		}
	}
	return rels
}

// permsOf turns held relations into resource's permissions.
func permsOf(held map[string]bool, resource string) accounts.Perms {
	var ps []accounts.Permission
	for _, p := range accounts.AllPermissions() {
		if p.Resource() == resource && held[Relation(p)] {
			ps = append(ps, p)
		}
	}
	return accounts.PermsOf(ps...)
}

// resolve answers every relation for every object: in BatchCheck calls
// when there are few checks, otherwise one ListObjects call per relation.
// A relation whose list was cut is checked object by object, up to
// maxFallbackRepos objects; more refuses.
func resolve(ctx context.Context, ck Checker, user string, rels, objects []string) (map[string]map[string]bool, error) {
	if len(rels) == 0 || len(objects) == 0 {
		return map[string]map[string]bool{}, nil
	}
	if len(rels)*len(objects) <= maxBatchChecks {
		return batch(ctx, ck, user, rels, objects)
	}
	want := map[string]bool{}
	for _, o := range objects {
		want[o] = true
	}
	out := map[string]map[string]bool{}
	var mu sync.Mutex
	err := run(ctx, len(rels), func(ctx context.Context, i int) error {
		rel := rels[i]
		got, err := ck.ListObjects(ctx, user, rel, fga.TypeRepository)
		if errors.Is(err, fga.ErrTruncated) {
			if len(objects) > maxFallbackRepos {
				return err
			}
			one, err := batch(ctx, ck, user, []string{rel}, objects)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			for obj, held := range one {
				if held[rel] {
					mark(out, obj, rel)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		for _, obj := range got {
			if want[obj] {
				mark(out, obj, rel)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func mark(out map[string]map[string]bool, obj, rel string) {
	if out[obj] == nil {
		out[obj] = map[string]bool{}
	}
	out[obj][rel] = true
}

// batch asks every relation for every object in BatchCheck calls of at
// most fga.MaxBatchCheck checks.
func batch(ctx context.Context, ck Checker, user string, rels, objects []string) (map[string]map[string]bool, error) {
	var checks []fga.Tuple
	for _, obj := range objects {
		for _, rel := range rels {
			checks = append(checks, fga.Tuple{User: user, Relation: rel, Object: obj})
		}
	}
	out := map[string]map[string]bool{}
	var mu sync.Mutex
	chunks := (len(checks) + fga.MaxBatchCheck - 1) / fga.MaxBatchCheck
	err := run(ctx, chunks, func(ctx context.Context, i int) error {
		part := checks[i*fga.MaxBatchCheck : min((i+1)*fga.MaxBatchCheck, len(checks))]
		got, err := ck.BatchCheck(ctx, part)
		if err != nil {
			return err
		}
		if len(got) != len(part) {
			return fga.ErrBadAnswer
		}
		mu.Lock()
		defer mu.Unlock()
		for j, ok := range got {
			if ok {
				mark(out, part[j].Object, part[j].Relation)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// run calls f for 0..n-1 with at most parallel calls at once and returns
// the first error; the others are cancelled.
func run(ctx context.Context, n int, f func(context.Context, int) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := f(ctx, i); err != nil {
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}
