package telemetry

import (
	"sync"
	"time"
)

// PoolStat is one connection pool's state as the pool reports it.
type PoolStat struct {
	Idle, InUse, Max int
	// Cumulative since the pool opened.
	EmptyAcquires    int64
	EmptyAcquireWait time.Duration
}

// DBSystemPostgres is the one db_system that reports pool statistics.
const DBSystemPostgres = "postgres"

type poolSource struct {
	system string
	stat   func() PoolStat
}

var pools = struct {
	sync.Mutex
	next int
	src  map[int]poolSource
}{src: map[int]poolSource{}}

// TrackPool lets the runtime sampler read a pool until untrack is called.
// Only the fixed db_system values are accepted; others are ignored.
func TrackPool(system string, stat func() PoolStat) (untrack func()) {
	if system != DBSystemPostgres || stat == nil {
		return func() {}
	}
	pools.Lock()
	id := pools.next
	pools.next++
	pools.src[id] = poolSource{system, stat}
	pools.Unlock()
	return sync.OnceFunc(func() {
		pools.Lock()
		delete(pools.src, id)
		pools.Unlock()
	})
}

// PoolStats sums the tracked pools per db_system. A process may open more
// than one pool to the same database (tests, migrations); the sum is what
// the process holds.
func PoolStats() map[string]PoolStat {
	pools.Lock()
	src := make([]poolSource, 0, len(pools.src))
	for _, s := range pools.src {
		src = append(src, s)
	}
	pools.Unlock()
	out := map[string]PoolStat{}
	for _, s := range src {
		st, sum := s.stat(), out[s.system]
		sum.Idle += st.Idle
		sum.InUse += st.InUse
		sum.Max += st.Max
		sum.EmptyAcquires += st.EmptyAcquires
		sum.EmptyAcquireWait += st.EmptyAcquireWait
		out[s.system] = sum
	}
	return out
}
