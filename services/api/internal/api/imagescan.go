package api

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/imagescan"
	"github.com/astrona-io/codesynapse/internal/model"
)

// Container image scans uploaded from CI: the JSON report of Trivy or Grype
// for the images a repository builds, so base-image CVEs reach the security
// view beside the dependency ones. Reports are untrusted (internal/imagescan
// bounds and validates them); only the parsed findings are kept, per
// organisation namespace in the accounts database, so single-tenant installs
// without accounts answer 501.

func (s *Server) imageScanRoutes() {
	s.route("GET /api/v1/repos/{repo}/images", repoRole(accounts.Viewer), s.handleImageScan)
	s.route("PUT /api/v1/repos/{repo}/images", repoRole(accounts.Member), s.handleUploadImageScan)
	s.route("DELETE /api/v1/repos/{repo}/images", repoRole(accounts.Member), s.handleDeleteImageScan)
}

// ImageScanSummary is what the API says about the uploaded scan: counts for
// the security report, findings (worst first, capped) for the images page.
type ImageScanSummary struct {
	imagescan.Overview
	Truncated bool                `json:"truncated"`
	Findings  []imagescan.Finding `json:"findings"`
}

const maxImageFindingsShown = 500

func summariseImages(sc accounts.ImageScan, withFindings bool) ImageScanSummary {
	out := ImageScanSummary{Overview: imagescan.Summarise(sc.UploadedAt, sc.Findings), Findings: []imagescan.Finding{}}
	if !withFindings {
		return out
	}
	ranked := append([]imagescan.Finding(nil), sc.Findings...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return model.SeverityRank(ranked[i].Severity) > model.SeverityRank(ranked[j].Severity)
	})
	if len(ranked) > maxImageFindingsShown {
		ranked, out.Truncated = ranked[:maxImageFindingsShown], true
	}
	out.Findings = ranked
	return out
}

func (s *Server) imageScanStore(w http.ResponseWriter) (accounts.Store, bool) {
	if s.cfg.Accounts == nil {
		s.problem(w, http.StatusNotImplemented, errors.New("image scans need accounts mode"))
		return nil, false
	}
	return s.cfg.Accounts.St, true
}

func (s *Server) handleImageScan(w http.ResponseWriter, r *http.Request) {
	st, ok := s.imageScanStore(w)
	if !ok {
		return
	}
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	sc, has, err := st.ImageScan(r.Context(), ns)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if !has {
		s.problem(w, http.StatusNotFound, errors.New("no image scan uploaded; send a Trivy or Grype JSON report with codesynapse images upload"))
		return
	}
	writeJSON(w, 200, summariseImages(sc, true))
}

func (s *Server) handleUploadImageScan(w http.ResponseWriter, r *http.Request) {
	st, ok := s.imageScanStore(w)
	if !ok {
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, imagescan.MaxReportBytes)
	switch r.Header.Get("Content-Encoding") {
	case "":
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			s.problem(w, http.StatusBadRequest, fmt.Errorf("invalid gzip body: %w", err))
			return
		}
		defer zr.Close() //nolint:errcheck // read-only
		body = zr
	default:
		s.problem(w, http.StatusBadRequest, errors.New("content-encoding must be gzip or none"))
		return
	}
	findings, err := imagescan.Parse(body)
	if err != nil {
		code := http.StatusBadRequest
		var tooBig *http.MaxBytesError
		if errors.Is(err, imagescan.ErrTooLarge) || errors.As(err, &tooBig) {
			code = http.StatusRequestEntityTooLarge
		}
		s.problem(w, code, err)
		return
	}
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	sc := accounts.ImageScan{UploadedAt: time.Now().UTC(), Findings: findings}
	if err := st.SetImageScan(r.Context(), ns, sc); err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, summariseImages(sc, true))
}

func (s *Server) handleDeleteImageScan(w http.ResponseWriter, r *http.Request) {
	st, ok := s.imageScanStore(w)
	if !ok {
		return
	}
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	if err := st.DeleteImageScan(r.Context(), ns); err != nil {
		s.problem(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
