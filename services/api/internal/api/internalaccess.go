package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// Permission checks for processes that do not decide them (OpenFGA plan,
// phase 6): only the API holds the OpenFGA key, so the MCP server and the
// workers ask it here. Neither endpoint takes a service token.
//
//   - POST /api/v1/internal/access answers for the caller: their own API key
//     or browser session and the runtime session they work in, the same
//     identity rules as every other route. The MCP server forwards the
//     calling agent's own key.
//   - POST /api/v1/internal/jobs/{id}/access answers for a durable job: the
//     worker proves it holds the job's lease, and the API rebuilds the
//     requester's context from the stored job (jobContext: session, account,
//     key, membership and permission checked again) and answers only for
//     that requester, organisation and repository.
//
// Both take one organisation and at most accounts.MaxAccessGraphs graphs,
// are audited like every changing request, and refuse on any doubt.
const (
	callerAccessPath = "POST /api/v1/internal/access"
	jobAccessPath    = "POST /api/v1/internal/jobs/{id}/access"
	// JobLeaseHeader carries the lease owner of the process running a job.
	JobLeaseHeader = "X-CodeSynapse-Job-Lease"
)

func (s *Server) internalAccessRoutes() {
	s.route(callerAccessPath, anyone, s.handleCallerAccess)
	// Authenticated by the job lease in the handler: the request carries no
	// account credentials, and authenticate lets only this path through
	// without them (jobLeaseRequest).
	s.account(jobAccessPath, businessRoute, s.handleJobAccess)
}

// jobLeaseRequest reports whether r is a worker's job access question, which
// authenticate passes on anonymously: the handler checks the lease.
func jobLeaseRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/internal/jobs/") &&
		strings.HasSuffix(r.URL.Path, "/access") && r.Header.Get(JobLeaseHeader) != ""
}

func (s *Server) handleCallerAccess(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Accounts == nil {
		s.problem(w, http.StatusNotFound, errors.New("accounts are off"))
		return
	}
	p, ok := principal(r.Context())
	if !ok || (p.Via != "key" && p.Via != "session") {
		s.problem(w, http.StatusForbidden, errors.New("access checks need your own API key or session"))
		return
	}
	var q accounts.AccessQuery
	if err := decodeJSON(w, r, &q); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := q.Validate(); err != nil {
		s.accountError(w, err)
		return
	}
	// The runtime session must be the caller's, in the organisation asked
	// about; a browser without one gets its dashboard binding (guard).
	id, err := execution.Require(r.Context())
	if err != nil || id.SubjectID != p.User.ID || id.TenantID != q.OrgID {
		s.problem(w, http.StatusForbidden, errors.New("access checks need a runtime session of yours in that organisation"))
		return
	}
	a, err := s.cfg.Accounts.AccessIn(r.Context(), p, q)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// jobAccessRequest is a worker's question about the job it runs. The
// identity is repeated so a mix-up is refused, never answered for someone
// else.
type jobAccessRequest struct {
	UserID string `json:"user_id"`
	Via    string `json:"via"`
	KeyID  string `json:"key_id,omitempty"`
	accounts.AccessQuery
}

func (s *Server) handleJobAccess(w http.ResponseWriter, r *http.Request) {
	refuse := func() { s.problem(w, http.StatusForbidden, errors.New("no running job of yours has that lease")) }
	if s.cfg.Accounts == nil || s.jobs.store == nil {
		s.problem(w, http.StatusNotFound, errors.New("durable jobs are off"))
		return
	}
	if _, signedIn := principal(r.Context()); signedIn {
		refuse() // the lease is the only credential this route takes
		return
	}
	lease := r.Header.Get(JobLeaseHeader)
	var req jobAccessRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if err := req.Validate(); err != nil {
		s.accountError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rec, err := s.jobs.store.JobByID(ctx, r.PathValue("id"))
	if err != nil || !leaseHeld(rec, lease, time.Now()) {
		refuse()
		return
	}
	if req.UserID != rec.ActorID || req.Via != rec.Via || req.KeyID != rec.KeyID || req.OrgID != rec.OrgID {
		refuse()
		return
	}
	for _, g := range req.Graphs {
		if g != rec.Repo {
			refuse()
			return
		}
	}
	if !req.OrgOnly && len(req.Graphs) == 0 {
		req.Graphs = []string{rec.Repo} // never more than the job's repository
	}
	jobCtx, err := s.jobContext(ctx, rec)
	if err != nil {
		s.recordJobDenial(ctx, accounts.Event{UserID: rec.ActorID, Via: rec.Via, KeyID: rec.KeyID,
			OrgID: rec.OrgID, Action: "job.authorization_denied", Target: rec.Repo, Outcome: accounts.Failure, Status: http.StatusForbidden})
		s.problem(w, http.StatusForbidden, errors.New("the requester no longer has access"))
		return
	}
	p, _ := principal(jobCtx)
	a, err := s.cfg.Accounts.AccessIn(jobCtx, p, req.AccessQuery)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// leaseHeld reports whether owner holds rec's lease now. Owners are random
// per process (newOwner) and never leave the accounts database or the
// process that holds them.
func leaseHeld(rec accounts.JobRecord, owner string, now time.Time) bool {
	return rec.Active() && rec.LeaseOwner != "" && len(owner) == len(rec.LeaseOwner) &&
		subtle.ConstantTimeCompare([]byte(owner), []byte(rec.LeaseOwner)) == 1 && rec.LeaseUntil.After(now)
}
