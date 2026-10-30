package api

import (
	"cmp"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/sarifimport"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Findings of external security tools imported from SARIF (docs/plans/
// SARIF_IMPORT_PLAN.md): CI uploads a tool's SARIF log for a commit, the
// log is read by the bounded parser in internal/sarifimport and only what
// it maps (tool, rule, severity, location, a short redacted message) is
// kept, per organisation namespace beside the code graph like runtime
// facts. Each tool's latest upload replaces its earlier one.

// maxSARIFUpload bounds the request body as sent (gzip or plain); the
// parser bounds the log it holds.
const maxSARIFUpload = sarifimport.MaxLogBytes

func (s *Server) sarifRoutes() {
	s.route("GET /api/v1/repos/{repo}/sarif", repoRole(accounts.Viewer), s.handleSARIF)
	s.route("PUT /api/v1/repos/{repo}/sarif", repoRole(accounts.Member), s.handleUploadSARIF)
	s.route("DELETE /api/v1/repos/{repo}/sarif", repoRole(accounts.Member), s.handleDeleteSARIF)
}

// SARIFTool is what the API says about one tool's latest upload.
type SARIFTool struct {
	Tool       string         `json:"tool"`
	Version    string         `json:"version,omitempty"`
	Commit     string         `json:"commit"`
	UploadedAt time.Time      `json:"uploaded_at"`
	Findings   int            `json:"findings"`
	BySeverity map[string]int `json:"by_severity"`
	Suppressed int            `json:"suppressed"`
	// Dropped counts results with no place in the repository, Unmatched
	// those at absolute paths no indexed file matched.
	Dropped   int `json:"dropped"`
	Unmatched int `json:"unmatched"`
	// Stale is set when the tool scanned another commit than the graph.
	Stale bool `json:"stale"`
}

// SARIFFinding is one imported finding with the tool that flagged it.
type SARIFFinding struct {
	sarifimport.Finding
	Tool string `json:"tool"`
	// FoundBy names the tool and its version, for a "found by" label.
	FoundBy string `json:"found_by"`
}

// SARIFReport answers GET /sarif.
type SARIFReport struct {
	Tools []SARIFTool `json:"tools"`
	// Total is how many findings match the filters; Findings holds the
	// first of them, most severe first.
	Total    int            `json:"total"`
	Findings []SARIFFinding `json:"findings"`
}

func sarifTools(set sarifimport.Set, head string) []SARIFTool {
	out := make([]SARIFTool, 0, len(set.Runs))
	for _, r := range set.Runs {
		t := SARIFTool{Tool: r.Tool, Version: r.Version, Commit: r.Commit, UploadedAt: r.UploadedAt, Findings: len(r.Findings),
			BySeverity: map[string]int{}, Dropped: r.Dropped, Unmatched: r.Unmatched,
			Stale: runtimefacts.Facts{Commit: r.Commit}.StaleFor(head)}
		for _, f := range r.Findings {
			t.BySeverity[f.Severity]++
			if f.Suppressed {
				t.Suppressed++
			}
		}
		out = append(out, t)
	}
	return out
}

func foundBy(r sarifimport.StoredRun) string {
	if r.Version == "" {
		return r.Tool
	}
	return r.Tool + " " + r.Version
}

func (s *Server) sarifStore(w http.ResponseWriter) (store.SARIFStore, bool) {
	ss, ok := s.st.(store.SARIFStore)
	if !ok {
		s.problem(w, http.StatusNotImplemented, errors.New("this graph store keeps no imported findings"))
	}
	return ss, ok
}

// sarifSet reads the namespace's findings, an empty set when there are none.
func sarifSet(r *http.Request, ss store.SARIFStore, ns string) (sarifimport.Set, error) {
	set, err := ss.SARIFFindings(r.Context(), ns)
	if errors.Is(err, store.ErrNotFound) {
		return sarifimport.Set{}, nil
	}
	return set, err
}

func (s *Server) handleSARIF(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	repoNode, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	ss, ok := s.sarifStore(w)
	if !ok {
		return
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	set, err := sarifSet(r, ss, ns)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	q := r.URL.Query()
	tool, minSev := q.Get("tool"), q.Get("min_severity")
	if minSev != "" && model.SeverityRank(minSev) < 0 {
		s.problem(w, http.StatusBadRequest, errors.New("min_severity must be info, low, medium, high or critical"))
		return
	}
	limit := min(intParam(r, "limit", 100), store.MaxLimit)
	out := SARIFReport{Tools: sarifTools(set, repoNode.Head), Findings: []SARIFFinding{}}
	for _, run := range set.Runs {
		if tool != "" && !strings.EqualFold(tool, run.Tool) {
			continue
		}
		for _, f := range run.Findings {
			if minSev != "" && model.SeverityRank(f.Severity) < model.SeverityRank(minSev) {
				continue
			}
			out.Findings = append(out.Findings, SARIFFinding{Finding: f, Tool: run.Tool, FoundBy: foundBy(run)})
		}
	}
	slices.SortFunc(out.Findings, func(a, b SARIFFinding) int {
		return cmp.Or(cmp.Compare(model.SeverityRank(b.Severity), model.SeverityRank(a.Severity)), cmp.Compare(a.Tool, b.Tool),
			cmp.Compare(a.File, b.File), cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(a.Fingerprint, b.Fingerprint))
	})
	out.Total = len(out.Findings)
	out.Findings = out.Findings[:min(len(out.Findings), limit)]
	writeJSON(w, 200, out)
}

func (s *Server) handleUploadSARIF(w http.ResponseWriter, r *http.Request) {
	ctx, repo := r.Context(), r.PathValue("repo")
	repoNode, err := s.st.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	ss, ok := s.sarifStore(w)
	if !ok {
		return
	}
	commit := r.URL.Query().Get("commit")
	if !commitRe.MatchString(commit) {
		s.problem(w, http.StatusBadRequest, errors.New("commit must be the hexadecimal commit the tool scanned"))
		return
	}
	body, err := sarifBody(w, r)
	if err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	defer body.Close() //nolint:errcheck // read-only
	runs, err := sarifimport.Parse(body)
	if err != nil {
		code := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.Is(err, sarifimport.ErrTooLarge) || errors.As(err, &tooBig) {
			code = http.StatusRequestEntityTooLarge
		}
		s.problem(w, code, err)
		return
	}
	now, files := time.Now().UTC(), store.RuntimeSource(s.st, repo)
	up := make([]sarifimport.StoredRun, 0, len(runs))
	for _, run := range runs {
		unmatched, err := sarifimport.Place(ctx, files, &run)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		up = append(up, sarifimport.StoredRun{Run: run, Commit: strings.ToLower(commit), UploadedAt: now, Unmatched: unmatched})
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	// Other tools' uploads are kept. Two uploads at once may each miss the
	// other's tool; uploading again restores it.
	prev, err := sarifSet(r, ss, ns)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	set := prev.With(up)
	if len(set.Runs) > sarifimport.MaxRuns || set.Total() > sarifimport.MaxStored {
		s.problem(w, http.StatusRequestEntityTooLarge, fmt.Errorf("at most %d tools and %d findings are kept per repository; delete a tool's findings first: %w",
			sarifimport.MaxRuns, sarifimport.MaxStored, sarifimport.ErrTooLarge))
		return
	}
	if err := ss.SetSARIFFindings(ctx, ns, set); err != nil {
		if errors.Is(err, runtimefacts.ErrTooLarge) {
			s.problem(w, http.StatusRequestEntityTooLarge, err)
			return
		}
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, SARIFReport{Tools: sarifTools(set, repoNode.Head), Total: set.Total(), Findings: []SARIFFinding{}})
}

// handleDeleteSARIF drops one tool's findings (?tool=) or all of them.
func (s *Server) handleDeleteSARIF(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	ss, ok := s.sarifStore(w)
	if !ok {
		return
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	tool := r.URL.Query().Get("tool")
	if tool == "" {
		if err := ss.DeleteSARIFFindings(r.Context(), ns); err != nil {
			s.problem(w, 500, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	set, err := sarifSet(r, ss, ns)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	rest, had := set.Without(tool)
	if !had {
		s.problem(w, http.StatusNotFound, errors.New("no findings of that tool"))
		return
	}
	if len(rest.Runs) == 0 {
		err = ss.DeleteSARIFFindings(r.Context(), ns)
	} else {
		err = ss.SetSARIFFindings(r.Context(), ns, rest)
	}
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sarifBody is the uploaded log, gzip-compressed or not, within its limit.
func sarifBody(w http.ResponseWriter, r *http.Request) (io.ReadCloser, error) {
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") && !strings.HasPrefix(ct, "application/sarif+json") {
		return nil, errors.New("content-type must be application/sarif+json or application/json")
	}
	body := http.MaxBytesReader(w, r.Body, maxSARIFUpload)
	switch r.Header.Get("Content-Encoding") {
	case "":
		return body, nil
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("invalid gzip body: %w", err)
		}
		return zr, nil
	default:
		return nil, errors.New("content-encoding must be gzip or none")
	}
}
