package accounts

import (
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
)

// Organisation watches: a person asks CodeSynapse to add the public
// repositories an organisation or user on a git host creates later, so the
// import does not have to be repeated. The watch runs as the member who
// created it (Execution, ActorID) and is rechecked on every scan.

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
	Paused          bool
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
}

const (
	// MaxWatches bounds the watches due in one listing.
	MaxWatches = 1000
	// MaxWatchesPerOrg bounds the watches of one organisation.
	MaxWatchesPerOrg = 20
	// MaxWatchKnown bounds the seen list of one watch.
	MaxWatchKnown = 5000
)
