package accounts

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// Organisation watches: a person asks CodeSynapse to add the repositories an
// organisation or user on a git host creates later, so the import does not
// have to be repeated: public ones always, private ones too while the
// creator is an owner or admin and the organisation's Git token for the host
// reads them. The watch runs as the member who created it (Execution,
// ActorID) and is rechecked on every scan.

// OrgWatch is one watched organisation or user on a git host, within one
// CodeSynapse organisation.
type OrgWatch struct {
	OrgID     string
	ActorID   string
	Execution execution.Identity
	// Provider is github, forgejo or gitlab; Host and Login say where.
	Provider        string
	Host            string
	Login           string
	IncludeArchived bool
	IncludeForks    bool
	// IncludePrivate lists with the organisation's Git token, while the
	// creator is an owner or admin; only they can turn it on.
	IncludePrivate bool
	// PrivateSince is when IncludePrivate was last turned on, or the watch
	// passed to someone else with it on. A scan after it takes a new
	// private baseline.
	PrivateSince time.Time
	// Exclude holds lowercase name patterns (see WatchExcludes) the watch
	// never adds.
	Exclude []string
	Paused  bool
	// LastScan is when the last scan finished; NextScan when the next is due.
	LastScan   time.Time
	LastStatus string
	LastError  string
	NextScan   time.Time
	// Added counts repositories the watch has added so far.
	Added int
	// Known is the repositories the watch has already seen (owner/name as
	// listed), so a repository someone removed is not added again.
	Known []string
	// PrivateBaseline is when the watch first listed with the
	// organisation's Git token, or first did so after that token was
	// replaced; zero until then. The private repositories found on that
	// scan join Known instead of being added.
	PrivateBaseline time.Time
}

// Key identifies the watch inside its organisation: host/login, lowercase.
func (w OrgWatch) Key() string { return WatchKey(w.Host, w.Login) }

// WatchKey builds the key of a watch.
func WatchKey(host, login string) string { return strings.ToLower(host + "/" + login) }

// WatchScan records how a scan went and when the next is due.
type WatchScan struct {
	LastScan   time.Time
	LastStatus string
	LastError  string
	NextScan   time.Time
	// Added is the number of repositories this scan added.
	Added int
	// Known replaces the seen list.
	Known []string
	// PrivateBaseline records a new private baseline; zero keeps the old one.
	PrivateBaseline time.Time
}

const (
	// MaxWatches bounds the watches due in one listing.
	MaxWatches = 1000
	// MaxWatchesPerOrg bounds the watches of one organisation.
	MaxWatchesPerOrg = 20
	// MaxWatchKnown bounds the seen list of one watch.
	MaxWatchKnown = 5000
	// MaxWatchExcludes bounds the name patterns of one watch.
	MaxWatchExcludes = 20
	// MaxWatchPattern bounds the length of one name pattern.
	MaxWatchPattern = 100
)

// WatchExcludes validates name patterns for a watch and returns them
// lowercased, sorted and without duplicates. A pattern is a repository name
// in which "*" stands for any run of characters (test-*, *-archive,
// *legacy*); the other characters are those of a repository name (letters,
// digits, '-', '_', '.'). A pattern of only "*" is refused: pausing the
// watch does that.
func WatchExcludes(patterns []string) ([]string, error) {
	if len(patterns) > MaxWatchExcludes {
		return nil, fmt.Errorf("%w: at most %d patterns", ErrInvalidInput, MaxWatchExcludes)
	}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || len(p) > MaxWatchPattern {
			return nil, fmt.Errorf("%w: a pattern is 1 to %d characters", ErrInvalidInput, MaxWatchPattern)
		}
		if strings.Trim(p, "*") == "" {
			return nil, fmt.Errorf("%w: a pattern needs more than \"*\"; pause the watch to add nothing", ErrInvalidInput)
		}
		for _, c := range p {
			if c != '*' && c != '-' && c != '_' && c != '.' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
				return nil, fmt.Errorf("%w: a pattern holds letters, digits, '-', '_', '.' and '*' only", ErrInvalidInput)
			}
		}
		out = append(out, p)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// WatchExcluded says whether name matches one of the patterns, ignoring
// case.
func WatchExcluded(patterns []string, name string) bool {
	name = strings.ToLower(name)
	return slices.ContainsFunc(patterns, func(p string) bool { return starMatch(p, name) })
}

// starMatch matches s against p, where '*' is any run of characters and
// every other byte stands for itself. Greedy with backtracking to the last
// star, so it is linear in practice and never recursive.
func starMatch(p, s string) bool {
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi, mark = star+1, mark+1
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
