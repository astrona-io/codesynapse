package fgasync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/logx"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Bounds.
const (
	// MaxObjectTuples bounds what one read of one object may return; a
	// larger answer fails the sync rather than acting on part of it.
	MaxObjectTuples = 2 * accounts.MaxTuples
	// MaxOrgObjects bounds the teams and repositories one organisation
	// sync reads.
	MaxOrgObjects = accounts.MaxTuples
	// readers is how many object reads run at once.
	readers = 8
)

// ErrStale means OpenFGA has not caught up with an organisation's policy:
// phase 6 refuses the check rather than answer from old tuples.
var ErrStale = errors.New("openfga: organisation policy not synced yet")

// Client is the part of fga.Client the sync uses.
type Client interface {
	Read(ctx context.Context, filter fga.Tuple, maxTuples int) ([]fga.Tuple, error)
	WriteTuples(ctx context.Context, tuples []fga.Tuple) error
	DeleteTuples(ctx context.Context, tuples []fga.Tuple) error
	StoreID() string
	ModelID() string
}

// Syncer writes organisations' policy to OpenFGA. Only one process may
// write at a time: the API runs it under the api-loops lease.
type Syncer struct {
	Client  Client
	Records accounts.OpenFGASyncStore
	// Policy reads one organisation's policy, normally
	// (*accounts.Service).OrgPolicy.
	Policy func(ctx context.Context, orgID string) (accounts.OrgPolicy, error)
	Now    func() time.Time
}

// Result is what one sync or verification found.
type Result struct {
	OrgID    string `json:"org_id"`
	Revision int64  `json:"revision"`
	// Added and Deleted count the tuples written and deleted; for Verify,
	// the tuples OpenFGA lacks and the ones it should not hold.
	Added   int `json:"added"`
	Deleted int `json:"deleted"`
}

// Drift reports whether OpenFGA differed from PostgreSQL.
func (r Result) Drift() bool { return r.Added+r.Deleted > 0 }

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Sync makes OpenFGA hold exactly orgID's policy and records the revision
// it accepted. It is idempotent: a sync cut short by an error or a lost
// lease leaves the old revision recorded, and the next one finishes the
// job from what OpenFGA then holds.
func (s *Syncer) Sync(ctx context.Context, orgID string) (Result, error) {
	started := s.now()
	res, add, del, err := s.diff(ctx, orgID)
	if err != nil {
		telemetry.RecordOpenFGASync(ctx, telemetry.OpenFGAError)
		return res, err
	}
	if err := s.apply(ctx, add, del); err != nil {
		telemetry.RecordOpenFGASync(ctx, telemetry.OpenFGAError)
		return res, err
	}
	rec := accounts.OpenFGASync{OrgID: orgID, StoreID: s.Client.StoreID(), ModelID: s.Client.ModelID(), Revision: res.Revision, SyncedAt: s.now()}
	if err := s.Records.RecordOpenFGASync(ctx, rec, started); err != nil {
		telemetry.RecordOpenFGASync(ctx, telemetry.OpenFGAError)
		return res, fmt.Errorf("record openfga sync: %w", err)
	}
	telemetry.RecordOpenFGASync(ctx, telemetry.OpenFGAOK)
	return res, nil
}

// Verify compares OpenFGA with orgID's policy and changes nothing.
func (s *Syncer) Verify(ctx context.Context, orgID string) (Result, error) {
	res, _, _, err := s.diff(ctx, orgID)
	return res, err
}

// SyncPending syncs at most limit organisations OpenFGA is behind on, one
// after another. An organisation that fails is logged and stays pending;
// the others go ahead. It returns how many it synced.
func (s *Syncer) SyncPending(ctx context.Context, limit int) (int, error) {
	orgs, err := s.Records.OpenFGAPending(ctx, s.Client.StoreID(), s.Client.ModelID(), limit)
	if err != nil {
		return 0, fmt.Errorf("pending openfga syncs: %w", err)
	}
	telemetry.OpenFGAPendingOrgs.SetContext(ctx, float64(len(orgs)))
	done := 0
	for _, o := range orgs {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		res, err := s.Sync(ctx, o.ID)
		if err != nil {
			logx.FromContext(ctx).Warn("openfga sync failed", "org", o.ID, logx.Error(err))
			continue
		}
		if res.Drift() {
			logx.FromContext(ctx).Info("openfga synced", "org", o.ID, "revision", res.Revision, "added", res.Added, "deleted", res.Deleted)
		}
		done++
	}
	return done, nil
}

// Fresh returns ErrStale unless OpenFGA holds org's current policy revision
// in the client's store and model. Phase 6 calls it before every check.
func Fresh(ctx context.Context, records accounts.OpenFGASyncStore, storeID, modelID string, org accounts.Org) error {
	rec, err := records.OpenFGASynced(ctx, org.ID)
	if errors.Is(err, accounts.ErrNotFound) {
		return ErrStale
	}
	if err != nil {
		return err
	}
	if storeID == "" || rec.StoreID != storeID || rec.ModelID != modelID || rec.Revision < org.PolicyRevision {
		return ErrStale
	}
	return nil
}

// diff returns the tuples to add and delete for orgID.
func (s *Syncer) diff(ctx context.Context, orgID string) (Result, []fga.Tuple, []fga.Tuple, error) {
	res := Result{OrgID: orgID}
	p, err := s.Policy(ctx, orgID)
	if err != nil {
		return res, nil, nil, fmt.Errorf("read policy: %w", err)
	}
	res.Revision = p.Org.PolicyRevision
	want, err := Desired(p)
	if err != nil {
		return res, nil, nil, err
	}
	have, err := s.current(ctx, orgID, want)
	if err != nil {
		return res, nil, nil, err
	}
	wantSet := map[fga.Tuple]bool{}
	for _, t := range want {
		wantSet[t] = true
	}
	haveSet := map[fga.Tuple]bool{}
	for _, t := range have {
		haveSet[t] = true
	}
	var add, del []fga.Tuple
	for _, t := range want {
		if !haveSet[t] {
			add = append(add, t)
		}
	}
	for t := range haveSet {
		if !wantSet[t] {
			del = append(del, t)
		}
	}
	sortTuples(add)
	sortTuples(del)
	res.Added, res.Deleted = len(add), len(del)
	return res, add, del, nil
}

// current reads every tuple OpenFGA holds for orgID: those on the
// organisation, and those on each team and repository anchored to it or
// that the policy names.
func (s *Syncer) current(ctx context.Context, orgID string, want []fga.Tuple) ([]fga.Tuple, error) {
	org, err := object(fga.TypeOrganisation, orgID)
	if err != nil {
		return nil, err
	}
	have, err := s.Client.Read(ctx, fga.Tuple{Object: org}, MaxObjectTuples)
	if err != nil {
		return nil, err
	}
	objects := map[string]bool{}
	for _, typ := range []string{fga.TypeTeam, fga.TypeRepository} {
		anchors, err := s.Client.Read(ctx, fga.Tuple{User: org, Relation: relOrganisation, Object: typ + ":"}, MaxOrgObjects)
		if err != nil {
			return nil, err
		}
		for _, t := range anchors {
			objects[t.Object] = true
		}
	}
	for _, t := range want {
		if t.Object != org {
			objects[t.Object] = true
		}
	}
	if len(objects) > 2*MaxOrgObjects {
		return nil, fga.ErrTruncated
	}
	list := make([]string, 0, len(objects))
	for o := range objects {
		list = append(list, o)
	}
	slices.Sort(list)

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first error
		sem   = make(chan struct{}, readers)
	)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, o := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(o string) {
			defer wg.Done()
			defer func() { <-sem }()
			got, err := s.Client.Read(rctx, fga.Tuple{Object: o}, MaxObjectTuples)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
					cancel()
				}
				return
			}
			have = append(have, got...)
		}(o)
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	return have, nil
}

// apply deletes, then adds, in batches of fga.MaxWrite. Anchors are
// deleted last and added first, so a sync stopped half-way never leaves
// tuples on an object the next sync cannot find.
func (s *Syncer) apply(ctx context.Context, add, del []fga.Tuple) error {
	split := func(ts []fga.Tuple) (anchors, rest []fga.Tuple) {
		for _, t := range ts {
			if anchor(t) {
				anchors = append(anchors, t)
			} else {
				rest = append(rest, t)
			}
		}
		return anchors, rest
	}
	delAnchors, delRest := split(del)
	addAnchors, addRest := split(add)
	steps := []struct {
		tuples []fga.Tuple
		write  bool
	}{{delRest, false}, {delAnchors, false}, {addAnchors, true}, {addRest, true}}
	for _, st := range steps {
		for batch := range slices.Chunk(st.tuples, fga.MaxWrite) {
			var err error
			if st.write {
				err = s.Client.WriteTuples(ctx, batch)
			} else {
				err = s.Client.DeleteTuples(ctx, batch)
			}
			if err != nil {
				return err
			}
			op := telemetry.OpenFGADelete
			if st.write {
				op = telemetry.OpenFGAWrite
			}
			telemetry.RecordOpenFGATuples(ctx, op, len(batch))
		}
	}
	return nil
}

func sortTuples(ts []fga.Tuple) {
	slices.SortFunc(ts, func(a, b fga.Tuple) int {
		return cmp.Or(strings.Compare(a.Object, b.Object), strings.Compare(a.Relation, b.Relation), strings.Compare(a.User, b.User))
	})
}
