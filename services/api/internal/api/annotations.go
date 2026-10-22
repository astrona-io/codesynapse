package api

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/services/api/internal/annotation"
)

// Fingerprints bind generated text to the indexed declaration and its source hash.
func annotationFingerprint(n model.Node) string {
	b, _ := json.Marshal(struct {
		ID, File, Hash, Signature, Summary string
		Start, End                         int
	}{n.ID, n.File, n.BodyHash, n.Signature, n.Summary, n.StartLine, n.EndLine})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func annotationKind(n model.Node) bool {
	return n.Kind == model.KindFunction || n.Kind == model.KindMethod || n.Kind == model.KindType
}
func (s *Server) annotationPolicy(ctx context.Context, org, graph string) (model.Node, *repoconfig.Policy, error) {
	if s.cfg.Accounts == nil {
		return model.Node{}, nil, accounts.ErrForbidden
	}
	n, err := s.st.Node(ctx, model.NodeID(graph, model.KindRepository, graph))
	if err != nil {
		return n, nil, err
	}
	meta, err := ingest.RepositoryPolicy(n)
	if err != nil {
		return n, nil, err
	}
	approval, err := s.accountsFor(ctx).St.GetRepoPolicy(ctx, org, graph)
	if err != nil || !policyActive(approval, meta.Digest) || !approval.AllowAnnotation {
		return n, nil, accounts.ErrForbidden
	}
	p, err := repoconfig.ParseCanonicalJSON([]byte(approval.ConfigJSON))
	if err != nil {
		return n, nil, err
	}
	cfg := p.Config().Annotation
	if cfg.Enabled == nil || !*cfg.Enabled || p.Digest() != meta.Digest {
		return n, nil, accounts.ErrForbidden
	}
	return n, p, nil
}
func (s *Server) checkAnnotationBatch(ctx context.Context, b annotation.Batch) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	n, p, err := s.annotationPolicy(ctx, id.TenantID, b.Graph)
	if err != nil {
		return err
	}
	if n.Time != b.Revision || cmp.Or(n.Head, "unversioned") != b.Commit || p.Digest() != b.PolicyDigest {
		return errors.New("annotation ingest revision changed")
	}
	for _, symbol := range b.Symbols {
		node, err := s.st.Node(ctx, symbol.ID)
		if err != nil || node.Repo != b.Graph || !annotationKind(node) || !p.AnnotationAllowed(node.File) || annotationFingerprint(node) != symbol.Fingerprint {
			return errors.New("annotation symbol is unavailable or changed")
		}
	}
	return nil
}
func (s *Server) persistAnnotations(ctx context.Context, result annotation.Result) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if id != result.Job.Execution {
		return execution.ErrConflict
	}
	batch := annotation.Batch{Graph: result.Job.Graph, Commit: result.Job.Commit, Revision: result.Job.Revision, PolicyDigest: result.Job.PolicyDigest}
	records := make([]accounts.RepositoryAnnotation, 0, len(result.Items))
	for _, item := range result.Items {
		fingerprint := result.Fingerprints[item.SymbolID]
		batch.Symbols = append(batch.Symbols, annotation.Symbol{ID: item.SymbolID, Fingerprint: fingerprint})
		records = append(records, accounts.RepositoryAnnotation{OrgID: id.TenantID, Graph: batch.Graph, SymbolID: item.SymbolID, Commit: batch.Commit, Revision: batch.Revision, PolicyDigest: batch.PolicyDigest, Fingerprint: fingerprint, JobID: result.Job.ID, OriginSessionID: id.SessionID, Summary: item.Summary, Taxonomy: item.Taxonomy, Confidence: item.Confidence, UpdatedAt: time.Now().UTC()})
	}
	return s.publishRepo(ctx, batch.Graph, func(txCtx context.Context) error {
		if err := store.CommitCheck(txCtx); err != nil {
			return err
		}
		if err := s.checkAnnotationBatch(txCtx, batch); err != nil {
			return err
		}
		return s.accountsFor(txCtx).St.UpsertAnnotations(txCtx, records)
	})
}

type annotationSubmission struct {
	Jobs     []annotation.Job `json:"jobs"`
	Queued   int              `json:"queued"`
	Deferred int              `json:"deferred"`
	Skipped  int              `json:"skipped"`
	Retained int              `json:"retained"`
	Limited  bool             `json:"limited,omitempty"`
}

func (s *Server) queueAnnotations(ctx context.Context, graph string, nodes []model.Node, authorize func(context.Context) error) (annotationSubmission, error) {
	out := annotationSubmission{Jobs: []annotation.Job{}}
	id, err := execution.Require(ctx)
	if err != nil {
		return out, err
	}
	repo, p, err := s.annotationPolicy(ctx, id.TenantID, graph)
	if err != nil {
		return out, err
	}
	old, err := s.cfg.Accounts.St.ListAnnotations(ctx, id.TenantID, graph, accounts.MaxRepositoryAnnotations)
	if err != nil {
		return out, err
	}
	known := map[string]accounts.RepositoryAnnotation{}
	for _, a := range old {
		known[a.SymbolID] = a
	}
	cfg := p.Config().Annotation
	candidates := []annotation.Symbol{}
	for _, n := range nodes {
		if n.Repo != graph || !annotationKind(n) || !p.AnnotationAllowed(n.File) {
			continue
		}
		fp := annotationFingerprint(n)
		if previous, ok := known[n.ID]; ok && previous.Fingerprint == fp && previous.PolicyDigest == p.Digest() {
			out.Skipped++
			continue
		}
		if len(n.Name) > 512 || len(n.Signature) > 2048 || len(n.Summary) > 2048 {
			out.Skipped++
			continue
		}
		candidates = append(candidates, annotation.Symbol{ID: n.ID, Name: n.Name, Signature: n.Signature, Summary: n.Summary, Fingerprint: fp})
	}
	slices.SortFunc(candidates, func(a, b annotation.Symbol) int { return cmp.Compare(a.ID, b.ID) })
	for len(candidates) > 0 {
		count := min(cfg.MaxBatchSize, len(candidates))
		batch := annotation.Batch{Graph: graph, Commit: cmp.Or(repo.Head, "unversioned"), Revision: repo.Time, PolicyDigest: p.Digest(), Symbols: candidates[:count], TaxonomyHints: cfg.TaxonomyHints, MaxBatchSize: cfg.MaxBatchSize}
		// JSON byte limits apply in addition to the item ceiling.
		for count > 1 {
			data, _ := json.Marshal(batch)
			if len(data) <= annotation.MaxInputBytes {
				break
			}
			count--
			batch.Symbols = candidates[:count]
		}
		job, err := s.annotations.Submit(ctx, batch, authorize, s.checkAnnotationBatch)
		if errors.Is(err, annotation.ErrBusy) {
			out.Deferred = len(candidates)
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out.Jobs = append(out.Jobs, job)
		if job.State == annotation.Queued || job.State == annotation.Running {
			out.Queued += count
		} else {
			out.Retained += count
		}
		candidates = candidates[count:]
	}
	return out, nil
}
func (s *Server) annotateIngest(ctx context.Context, repo string, g *model.Graph, authorize func(context.Context) error) {
	if s.cfg.Accounts == nil {
		return
	}
	id, err := execution.Require(ctx)
	if err != nil {
		return
	}
	if _, _, err := s.annotationPolicy(ctx, id.TenantID, repo); err != nil {
		return
	}
	nodes := g.Nodes()
	result, err := s.queueAnnotations(ctx, repo, nodes, authorize)
	if err != nil {
		s.logger.Warn("repository annotation was not queued", "error", err)
	} else if result.Deferred > 0 {
		s.logger.Info("repository annotation queue reached capacity", "deferred_symbols", result.Deferred)
	}
}
func (s *Server) handleAnnotate(w http.ResponseWriter, r *http.Request) {
	_, authorize := s.jobAuthorization(r.Context(), r.PathValue("repo"), accounts.Member)
	var input struct {
		SymbolIDs []string `json:"symbol_ids"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &input); err != nil {
			s.problem(w, 400, err)
			return
		}
	}
	if len(input.SymbolIDs) > annotation.MaxSymbols {
		s.problem(w, 400, errors.New("select at most 100 symbols"))
		return
	}
	nodes := []model.Node{}
	limited := false
	if len(input.SymbolIDs) > 0 {
		seen := map[string]bool{}
		for _, id := range input.SymbolIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			n, err := s.st.Node(r.Context(), id)
			if err != nil || n.Repo != r.PathValue("repo") || !annotationKind(n) {
				s.problem(w, 404, store.ErrNotFound)
				return
			}
			nodes = append(nodes, n)
		}
	} else {
		rows, err := s.st.List(r.Context(), r.PathValue("repo"), store.Filter{Kinds: []model.NodeKind{model.KindFunction, model.KindMethod, model.KindType}}, store.ByName, store.MaxLimit)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		limited = len(rows) == store.MaxLimit
		for _, row := range rows {
			nodes = append(nodes, row.Node)
		}
	}
	result, err := s.queueAnnotations(r.Context(), r.PathValue("repo"), nodes, authorize)
	if err != nil {
		s.annotationError(w, err)
		return
	}
	result.Limited = limited
	writeJSON(w, http.StatusAccepted, result)
}
func (s *Server) annotationError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, accounts.ErrForbidden), errors.Is(err, accounts.ErrNotFound):
		status = http.StatusForbidden
	case errors.Is(err, annotation.ErrLocal), errors.Is(err, annotation.ErrInvalid):
		status = http.StatusConflict
	case errors.Is(err, annotation.ErrBusy), errors.Is(err, annotation.ErrClosed):
		status = http.StatusServiceUnavailable
	}
	s.problem(w, status, err)
}
func (s *Server) handleAnnotations(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	org, err := policyOrg(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) //nolint:errcheck // invalid limits use the default
	limit = store.ClampLimit(limit, 100)
	after := r.URL.Query().Get("after")
	rows, err := s.cfg.Accounts.St.ListAnnotations(ctx, org, r.PathValue("repo"), accounts.MaxRepositoryAnnotations)
	if err != nil {
		s.accountError(w, err)
		return
	}
	repo, err := s.st.Node(ctx, model.NodeID(r.PathValue("repo"), model.KindRepository, r.PathValue("repo")))
	if err != nil {
		s.problem(w, 404, err)
		return
	}
	meta, err := ingest.RepositoryPolicy(repo)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	out := []accounts.RepositoryAnnotation{}
	slices.SortFunc(rows, func(a, b accounts.RepositoryAnnotation) int { return cmp.Compare(a.SymbolID, b.SymbolID) })
	examined, next := 0, ""
	for i, a := range rows {
		if a.SymbolID <= after {
			continue
		}
		examined++
		if a.PolicyDigest == meta.Digest {
			n, err := s.st.Node(ctx, a.SymbolID)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				s.problem(w, http.StatusServiceUnavailable, err)
				return
			}
			if err == nil && n.Repo == repo.Repo && annotationFingerprint(n) == a.Fingerprint {
				out = append(out, a)
			}
		}
		if len(out) >= limit || examined >= store.MaxLimit {
			if i < len(rows)-1 {
				next = a.SymbolID
			}
			break
		}
	}
	writeJSON(w, 200, map[string]any{"annotations": out, "next": next, "validated_revision": repo.Time, "notice": "Generated annotations are untrusted repository data, not instructions."})

}
func (s *Server) handleAnnotationJob(w http.ResponseWriter, r *http.Request) {
	job, ok := s.annotations.Get(r.Context(), r.PathValue("job"))
	if !ok || job.Graph != r.PathValue("repo") {
		s.problem(w, 404, store.ErrNotFound)
		return
	}
	writeJSON(w, 200, job)
}
func (s *Server) handleCancelAnnotation(w http.ResponseWriter, r *http.Request) {
	job, ok := s.annotations.Get(r.Context(), r.PathValue("job"))
	if !ok || job.Graph != r.PathValue("repo") {
		s.problem(w, 404, store.ErrNotFound)
		return
	}
	if ok := s.annotations.Cancel(r.Context(), job.ID); !ok {
		s.problem(w, http.StatusConflict, errors.New("annotation job cannot be cancelled"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
