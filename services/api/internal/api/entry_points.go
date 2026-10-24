package api

import (
	"errors"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/store"
)

// handleEntryPoints serves the Entry points page: the attack surface the
// attack_surface MCP tool answers, for one repository.
func (s *Server) handleEntryPoints(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	out, err := agentctx.Service{R: store.ForRepository(s.st, r.PathValue("repo"))}.AttackSurface(r.Context(), r.PathValue("repo"), agentctx.SurfaceOptions{
		Kind: q.Get("kind"), Reaching: q.Get("reaching"), Unguarded: q.Get("unguarded") == "true", Query: q.Get("q"),
		Limit: intParam(r, "limit", 200),
	})
	switch {
	case errors.Is(err, agentctx.ErrResponseBudget):
		s.problem(w, http.StatusUnprocessableEntity, err)
	case errors.Is(err, agentctx.ErrSurfaceFilter):
		s.problem(w, http.StatusBadRequest, err)
	case err != nil:
		s.problem(w, 500, err)
	default:
		writeJSON(w, 200, out)
	}
}
