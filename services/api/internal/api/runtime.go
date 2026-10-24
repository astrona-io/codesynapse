package api

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Runtime facts uploaded from CI (docs/PLAN.md, agent workflow roadmap step
// 11): coverage profiles and JUnit reports of a test run at a commit,
// parsed and mapped onto the graph here, kept per organisation namespace
// beside the code graph. Reports carry paths, line numbers, test names and
// counts; only the mapped counts are stored.

const (
	// maxRuntimeUpload bounds the request body as sent (gzip or plain) and
	// maxRuntimeJSON the JSON it holds.
	maxRuntimeUpload  = 64 << 20
	maxRuntimeJSON    = 128 << 20
	maxRuntimeReports = 20
	// Facts beyond these are refused rather than cut: a partial map would
	// call covered functions untested.
	maxRuntimeFunctions = 200_000
	maxRuntimeTests     = 200_000
)

var (
	commitRe     = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	reportNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func (s *Server) runtimeRoutes() {
	s.route("GET /api/v1/repos/{repo}/runtime", repoRole(accounts.Viewer), s.handleRuntimeFacts)
	s.route("PUT /api/v1/repos/{repo}/runtime", repoRole(accounts.Member), s.handleUploadRuntime)
	s.route("DELETE /api/v1/repos/{repo}/runtime", repoRole(accounts.Member), s.handleDeleteRuntime)
}

type runtimeReport struct {
	// Kind is "coverage" (Go profile or LCOV) or "junit".
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

type runtimeUpload struct {
	Commit  string          `json:"commit"`
	Reports []runtimeReport `json:"reports"`
}

// RuntimeSummary is what the API says about a namespace's facts.
type RuntimeSummary struct {
	Commit     string    `json:"commit"`
	MeasuredAt time.Time `json:"measured_at"`
	Origin     string    `json:"origin"`
	Sources    []string  `json:"sources"`
	Unit       string    `json:"unit,omitempty"`
	Functions  int       `json:"functions"`
	// Uncovered counts measured functions no test executed.
	Uncovered      int      `json:"uncovered"`
	Tests          int      `json:"tests"`
	Failed         int      `json:"failed"`
	Flaky          []string `json:"flaky"`
	UnmatchedFiles int      `json:"unmatched_files"`
	UnmatchedTests int      `json:"unmatched_tests"`
	// Stale is set when the facts measured another commit than the graph.
	Stale bool `json:"stale"`
}

func summarise(f runtimefacts.Facts, head, repo string) RuntimeSummary {
	out := RuntimeSummary{Commit: f.Commit, MeasuredAt: f.MeasuredAt, Origin: f.Origin, Sources: f.Sources, Unit: f.Unit,
		Functions: len(f.Coverage), Tests: len(f.Tests), Flaky: []string{}, UnmatchedFiles: f.UnmatchedFiles,
		UnmatchedTests: f.UnmatchedTests, Stale: f.StaleFor(head)}
	for _, c := range f.Coverage {
		if c.Covered == 0 {
			out.Uncovered++
		}
	}
	for id, t := range f.Tests {
		if t.Failed > 0 && !t.Flaky() {
			out.Failed++
		}
		if t.Flaky() {
			out.Flaky = append(out.Flaky, shortID(id, repo))
		}
	}
	slices.Sort(out.Flaky)
	out.Flaky = out.Flaky[:min(len(out.Flaky), 50)]
	return out
}

// shortID drops the repository and kind from a node ID.
func shortID(id, repo string) string {
	_, local, ok := strings.Cut(strings.TrimPrefix(id, repo+"|"), "|")
	if !ok {
		return id
	}
	return local
}

func (s *Server) runtimeStore(w http.ResponseWriter) (store.RuntimeStore, bool) {
	rs, ok := s.st.(store.RuntimeStore)
	if !ok {
		s.problem(w, http.StatusNotImplemented, errors.New("this graph store keeps no runtime facts"))
	}
	return rs, ok
}

func (s *Server) handleRuntimeFacts(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	repoNode, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	rs, ok := s.runtimeStore(w)
	if !ok {
		return
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	f, err := rs.RuntimeFacts(r.Context(), ns)
	if errors.Is(err, store.ErrNotFound) {
		s.problem(w, http.StatusNotFound, errors.New("no test run recorded; upload one with codesynapse runtime upload"))
		return
	}
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, summarise(f, repoNode.Head, repo))
}

func (s *Server) handleUploadRuntime(w http.ResponseWriter, r *http.Request) {
	ctx, repo := r.Context(), r.PathValue("repo")
	repoNode, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	rs, ok := s.runtimeStore(w)
	if !ok {
		return
	}
	in, err := decodeRuntimeUpload(w, r)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	var input runtimefacts.Input
	var sources []string
	for _, rep := range in.Reports {
		switch rep.Kind {
		case "coverage":
			p, err := runtimefacts.ParseCoverage(strings.NewReader(rep.Content))
			if err != nil {
				s.problem(w, http.StatusBadRequest, fmt.Errorf("%s: %w", rep.Name, err))
				return
			}
			input.Profiles = append(input.Profiles, p)
		case "junit":
			cases, err := runtimefacts.ParseJUnit(strings.NewReader(rep.Content))
			if err != nil {
				s.problem(w, http.StatusBadRequest, fmt.Errorf("%s: %w", rep.Name, err))
				return
			}
			input.Tests = append(input.Tests, cases...)
		}
		sources = append(sources, rep.Kind+" "+rep.Name)
	}
	f, err := runtimefacts.Map(ctx, store.RuntimeSource(s.st, repo), input)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if len(f.Coverage) > maxRuntimeFunctions || len(f.Tests) > maxRuntimeTests {
		s.problem(w, http.StatusRequestEntityTooLarge, runtimefacts.ErrTooLarge)
		return
	}
	slices.Sort(sources)
	f.Commit, f.MeasuredAt, f.Origin, f.Sources = strings.ToLower(in.Commit), time.Now().UTC(), "upload", sources
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	if err := rs.SetRuntimeFacts(ctx, ns, f); err != nil {
		if errors.Is(err, runtimefacts.ErrTooLarge) {
			s.problem(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, summarise(f, repoNode.Head, repo))
}

func (s *Server) handleDeleteRuntime(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	rs, ok := s.runtimeStore(w)
	if !ok {
		return
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	if err := rs.DeleteRuntimeFacts(r.Context(), ns); err != nil {
		s.problem(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeRuntimeUpload reads the upload, gzip-compressed or not, within its
// own limits (reports are larger than other bodies) and validates it.
func decodeRuntimeUpload(w http.ResponseWriter, r *http.Request) (runtimeUpload, error) {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return runtimeUpload{}, errors.New("content-type must be application/json")
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, maxRuntimeUpload)
	switch r.Header.Get("Content-Encoding") {
	case "":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return runtimeUpload{}, fmt.Errorf("invalid gzip body: %w", err)
		}
		defer zr.Close() //nolint:errcheck // read-only
		body = io.LimitReader(zr, maxRuntimeJSON)
	default:
		return runtimeUpload{}, errors.New("content-encoding must be gzip or none")
	}
	var in runtimeUpload
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return runtimeUpload{}, fmt.Errorf("invalid json body: %w", err)
	}
	if !commitRe.MatchString(in.Commit) {
		return runtimeUpload{}, errors.New("commit must be the hexadecimal commit the reports measured")
	}
	if len(in.Reports) == 0 || len(in.Reports) > maxRuntimeReports {
		return runtimeUpload{}, fmt.Errorf("send 1 to %d reports", maxRuntimeReports)
	}
	for i, rep := range in.Reports {
		if rep.Kind != "coverage" && rep.Kind != "junit" {
			return runtimeUpload{}, errors.New(`report kind must be "coverage" or "junit"`)
		}
		in.Reports[i].Name = path.Base(strings.ReplaceAll(rep.Name, "\\", "/"))
		if !reportNameRe.MatchString(in.Reports[i].Name) {
			return runtimeUpload{}, errors.New("report name must be a file name of letters, digits, dots, dashes and underscores")
		}
		if len(rep.Content) > runtimefacts.MaxReportBytes {
			return runtimeUpload{}, fmt.Errorf("%s: %w", in.Reports[i].Name, runtimefacts.ErrTooLarge)
		}
	}
	return in, nil
}
