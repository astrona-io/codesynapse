package duplicates

import (
	"cmp"
	"maps"
	"slices"
)

// A candidate for near-miss comparison: one distinct fingerprint, standing
// for every copy that has it.
type variant struct {
	sig string
	loc int
}

type cluster struct {
	members    []int   // indexes into the variants
	similarity float64 // lowest similarity of any edge that joined them
}

const (
	bands        = 8
	rowsPerBand  = SigSlots / bands
	maxBucket    = 50  // a bucket this full is noise, not a signal
	minSizeRatio = 0.5 // a function half the size of another is not an edited copy of it
	minNearLOC   = 10  // shorter functions share shape, not logic
)

// nearClusters groups variants whose signatures are similar enough to be
// edited copies of each other. Locality-sensitive hashing picks the pairs
// worth comparing, so thousands of variants stay cheap.
func nearClusters(vs []variant) []cluster {
	buckets := map[string][]int{}
	for i, v := range vs {
		if v.loc < minNearLOC || decodeSig(v.sig) == nil {
			continue
		}
		for b := range bands {
			// Four hex characters per slot.
			key := string(rune('a'+b)) + v.sig[b*rowsPerBand*4:(b+1)*rowsPerBand*4]
			buckets[key] = append(buckets[key], i)
		}
	}
	// Leader clustering: the largest variant leads, and a variant joins only if
	// it is close to the leader itself. Joining through a chain of neighbours
	// would glue together functions that merely share boilerplate.
	adj := map[int]map[int]float64{}
	seen := map[[2]int]bool{}
	for _, idx := range buckets {
		if len(idx) < 2 || len(idx) > maxBucket {
			continue
		}
		for x, i := range idx {
			for _, j := range idx[x+1:] {
				pair := [2]int{min(i, j), max(i, j)}
				if seen[pair] {
					continue
				}
				seen[pair] = true
				a, b := vs[i], vs[j]
				if float64(min(a.loc, b.loc)) < minSizeRatio*float64(max(a.loc, b.loc)) {
					continue
				}
				if sim := Similarity(a.sig, b.sig); sim >= NearThreshold {
					for _, e := range [][2]int{{i, j}, {j, i}} {
						if adj[e[0]] == nil {
							adj[e[0]] = map[int]float64{}
						}
						adj[e[0]][e[1]] = sim
					}
				}
			}
		}
	}
	order := make([]int, 0, len(adj))
	for i := range adj {
		order = append(order, i)
	}
	slices.SortFunc(order, func(a, b int) int { return cmp.Or(cmp.Compare(vs[b].loc, vs[a].loc), cmp.Compare(a, b)) })
	taken := map[int]bool{}
	var out []cluster
	for _, lead := range order {
		if taken[lead] {
			continue
		}
		c := cluster{members: []int{lead}, similarity: 1}
		for _, j := range slices.Sorted(maps.Keys(adj[lead])) {
			if !taken[j] {
				c.members = append(c.members, j)
				c.similarity = min(c.similarity, adj[lead][j])
			}
		}
		if len(c.members) < 2 {
			continue
		}
		for _, m := range c.members {
			taken[m] = true
		}
		slices.Sort(c.members)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b cluster) int { return a.members[0] - b.members[0] })
	return out
}
