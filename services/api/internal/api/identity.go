package api

import (
	"context"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

func (s *Server) handleIdentities(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	svc := query.Service{R: store.ForRepository(s.st, repo)}
	page, err := svc.Identities(r.Context(), repo, r.URL.Query().Get("id"), r.URL.Query().Get("review") == "1", intParam(r, "offset", 0), intParam(r, "limit", 20))
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleIdentityReviews(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	page, err := s.catalogOf(r.Context(), repo).IdentityReviews(r.Context(), repo, r.URL.Query().Get("symbol"), intParam(r, "offset", 0), intParam(r, "limit", 20))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleReviewIdentity(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	p, ok := principal(r.Context())
	if s.cfg.Accounts != nil && (!ok || p.Via != "session") {
		s.accountError(w, accounts.ErrForbidden)
		return
	}
	var in curation.IdentityReviewInput
	if err := decodeBody(w, r, &in); err != nil {
		s.catalogError(w, err)
		return
	}
	svc := s.catalogOf(r.Context(), repo)
	who := actorOf(r.Context(), repo)
	if s.cfg.Accounts != nil {
		svc.BeforeIdentityReview = func(ctx context.Context, target string) error {
			_, err := s.cfg.Accounts.Record(ctx, accounts.Event{UserID: p.User.ID, Via: p.Via, KeyID: p.KeyID, OrgID: who.Org, Action: "identity.review", Target: target, Outcome: accounts.Success, Status: http.StatusCreated})
			return err
		}
	}
	v, err := svc.ReviewIdentity(r.Context(), repo, who, in)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleCodeRecords(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	page, err := s.catalogOf(r.Context(), repo).CodeRecords(r.Context(), repo, r.URL.Query().Get("symbol"), intParam(r, "offset", 0), intParam(r, "limit", 20))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleRecordCode(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	var in curation.CodeInput
	if err := decodeBody(w, r, &in); err != nil {
		s.catalogError(w, err)
		return
	}
	v, err := s.catalogOf(r.Context(), repo).RecordCode(r.Context(), repo, actorOf(r.Context(), repo), in)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}
