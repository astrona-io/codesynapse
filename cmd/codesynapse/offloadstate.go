package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// offloadProgress remembers, per chat, how many older turns were offloaded.
// The API answers 202 before a job ran, so a request only counts once its job
// succeeded; until then it stays pending and a failure sends its turns again.
type offloadProgress struct {
	path    string
	done    int // turns whose job succeeded
	pending []offloadJob
}

type offloadJob struct {
	id    string
	turns int
}

// loadOffloadState reads the state file of a session. Without a session ID
// nothing is remembered. The file is "<done>" and one "<job id> <turns>" line
// per pending job; a bare number (the older format) has no pending jobs.
func loadOffloadState(session string) offloadProgress {
	if session == "" || strings.ContainsAny(session, `/\`) || len(session) > 128 {
		return offloadProgress{}
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return offloadProgress{}
	}
	p := filepath.Join(dir, "codesynapse", "offload", session)
	st := offloadProgress{path: p}
	b, err := os.ReadFile(p) //nolint:gosec // our own cache file, session ID checked above
	if err != nil {
		return st
	}
	for i, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if i == 0 {
			_, _ = fmt.Sscanf(line, "%d", &st.done) //nolint:errcheck // a broken file starts over
			continue
		}
		var j offloadJob
		if n, _ := fmt.Sscanf(line, "%s %d", &j.id, &j.turns); n == 2 && j.turns > 0 { //nolint:errcheck // skip broken lines
			st.pending = append(st.pending, j)
		}
	}
	return st
}

// sent is how many turns were handed to the API and not given up on.
func (s *offloadProgress) sent() int {
	n := s.done
	for _, j := range s.pending {
		n += j.turns
	}
	return n
}

func (s *offloadProgress) add(id string, turns int) {
	s.pending = append(s.pending, offloadJob{id: id, turns: turns})
}

// settle looks at the pending jobs in order. Succeeded ones move into done; a
// failed one is dropped with every job after it, so their turns go out again.
// Jobs still running stay pending.
func (s *offloadProgress) settle(state func(id string) string) {
	var keep []offloadJob
	for _, j := range s.pending {
		switch state(j.id) {
		case "succeeded":
			if len(keep) == 0 {
				s.done += j.turns
				continue
			}
			keep = append(keep, j)
		case "queued", "running":
			keep = append(keep, j)
		default: // failed or cancelled
			s.pending = keep
			return
		}
	}
	s.pending = keep
}

func (s *offloadProgress) save() {
	if s.path == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", s.done)
	for _, j := range s.pending {
		fmt.Fprintf(&b, "%s %d\n", j.id, j.turns)
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)        //nolint:errcheck // best effort
	_ = os.WriteFile(s.path, []byte(b.String()), 0o600) //nolint:errcheck // best effort
}
