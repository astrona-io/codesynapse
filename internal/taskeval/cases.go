package taskeval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Cases from agent sessions (docs/PLAN.md, agent workflow roadmap step 17):
// what a person asked an agent and the files the session then changed, kept
// on the machine that ran it when the person opts in. They test the tools on
// the questions people actually ask, next to the commit subjects of Tasks.

// Case is one session turned into a question with known answers.
type Case struct {
	// Session is a hash of the session ID: one case per session.
	Session string `json:"session"`
	// Task is the session's first prompt, redacted and shortened.
	Task string `json:"task"`
	// Base is the commit the session began at; the graph is built there.
	Base string `json:"base"`
	// Truth are the files the session changed that existed at Base.
	Truth []string `json:"truth"`
	// Added counts files the session created; no answer can name them.
	Added int       `json:"added,omitempty"`
	At    time.Time `json:"at"`
}

// AsTask is the case as an eval task.
func (c Case) AsTask() Task {
	return Task{Commit: c.Base, Subject: c.Task, Truth: c.Truth, Added: c.Added}
}

const (
	// MaxCases bounds the file: the newest are kept.
	MaxCases = 500
	// maxCaseFileBytes refuses a file that is not ours.
	maxCaseFileBytes = 8 << 20
)

// ReadCases reads a cases file (JSON Lines); a missing file has none.
func ReadCases(path string) ([]Case, error) {
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if st.Size() > maxCaseFileBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxCaseFileBytes)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the user's own cases file
	if err != nil {
		return nil, err
	}
	var out []Case
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var c Case
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if c.Task != "" && c.Base != "" && len(c.Truth) > 0 {
			out = append(out, c)
		}
	}
	return out, sc.Err()
}

// SaveCase adds c to the file, replacing the case of the same session (a
// session's Stop runs many times), and keeps the newest MaxCases.
func SaveCase(path string, c Case) error {
	cases, err := ReadCases(path)
	if err != nil {
		return err
	}
	cases = slices.DeleteFunc(cases, func(o Case) bool { return o.Session == c.Session })
	cases = append(cases, c)
	slices.SortStableFunc(cases, func(a, b Case) int { return a.At.Compare(b.At) })
	cases = cases[max(0, len(cases)-MaxCases):]
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, c := range cases {
		if err := enc.Encode(c); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Merge joins runs over different graphs (cases begun at different
// commits) into one report, with the summaries over all their results.
func Merge(repo string, parts []Report) Report {
	out := Report{Repo: repo, Summary: map[string]Summary{}}
	for _, p := range parts {
		out.Results = append(out.Results, p.Results...)
	}
	out.Tasks = len(out.Results)
	for _, tool := range []string{ToolMinimalContext, ToolSearch} {
		out.Summary[tool] = summarize(out.Results, tool)
	}
	return out
}
