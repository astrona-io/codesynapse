package api

import (
	"net/http"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/sbom"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// handleSBOM serves the repository's software bill of materials as
// CycloneDX 1.5 JSON, built from the stored dependency graph.
func (s *Server) handleSBOM(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	rd := store.ForRepository(s.st, repo)
	n, err := rd.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, 404, err)
		return
	}
	g, err := rd.DependencyGraph(r.Context(), repo)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	b, err := sbom.Marshal(sbom.Build(n, g, sbom.Options{Tool: s.cfg.Version}))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.cyclonedx+json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sbomFileName(repo)+`"`)
	_, _ = w.Write(b) //nolint:errcheck // client went away
}

// sbomFileName turns owner/name into a file name made of safe characters.
func sbomFileName(repo string) string { return sbomFileNameBase(repo) + ".cdx.json" }

// sbomFileNameBase turns a name into a file name made of safe characters.
func sbomFileNameBase(repo string) string {
	var b strings.Builder
	for _, c := range repo {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-.")
}
