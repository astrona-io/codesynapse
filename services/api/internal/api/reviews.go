package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/reviews"
)

func (s *Server) handleListReviews(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListReviews(r.Context(), r.PathValue("repo"), intParam(r, "limit", 100))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	sc := scopeFrom(r.Context())
	out := make([]reviews.Summary, 0, len(list))
	for _, rv := range list {
		if seesReview(sc, rv.Repo, rv.OrgID) {
			out = append(out, reviews.SummaryOf(rv))
		}
	}
	writeJSON(w, 200, map[string]any{"reviews": out})
}

func (s *Server) handleGetReview(w http.ResponseWriter, r *http.Request) {
	rv, err := s.st.GetReview(r.Context(), r.PathValue("id"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"review": rv, "counts": rv.Counts()})
}

func (s *Server) handleDeleteReview(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.GetReview(r.Context(), r.PathValue("id")); err != nil {
		s.problem(w, 500, err)
		return
	}
	if err := s.st.DeleteReview(r.Context(), r.PathValue("id")); err != nil {
		s.problem(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type commentStatusRequest struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

// handleUpdateReviewComment lets people resolve or reopen comments from the UI.
func (s *Server) handleUpdateReviewComment(w http.ResponseWriter, r *http.Request) {
	var req commentStatusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	rv, err := s.st.GetReview(r.Context(), r.PathValue("id"))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if err := reviews.ApplyStatus(&rv, r.PathValue("comment"), req.Status, req.Note, "web", time.Now()); err != nil {
		status := 500
		if errors.Is(err, reviews.ErrInvalid) {
			status = 400
		}
		s.problem(w, status, err)
		return
	}
	if err := s.st.SaveReview(r.Context(), rv); err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"review": rv, "counts": rv.Counts()})
}
