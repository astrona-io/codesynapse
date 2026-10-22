package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/services/api/internal/annotation"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

const annotationConfig = "version: 1\nannotation:\n  enabled: true\n  taxonomy_hints: [billing]\n"

func indexedPolicy(t *testing.T, f runtimeFixture, source string) *repoconfig.Policy {
	t.Helper()
	p, err := repoconfig.Parse([]byte(source))
	require.NoError(t, err)
	g := storetest.Fixture()
	n, ok := g.Node(model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo))
	require.True(t, ok)
	b, err := json.Marshal(map[string]any{"repository_policy": ingest.PolicyMetadata{Present: true, Digest: p.Digest(), Config: p.Config(), ExclusionsApproved: p.RequiresApproval()}})
	require.NoError(t, err)
	n.Meta = string(b)
	n.Time = "2026-10-22T10:00:00Z"
	require.NoError(t, f.store.ReplaceRepo(t.Context(), storetest.Repo, g))
	return p
}
func approvePolicy(t *testing.T, f runtimeFixture, p *repoconfig.Policy) {
	t.Helper()
	b, err := json.Marshal(p.Config())
	require.NoError(t, err)
	require.NoError(t, f.accounts.SetRepoPolicy(t.Context(), accounts.Principal{User: f.user, Via: "session"}, accounts.RepoPolicyApproval{OrgID: f.org.ID, Graph: storetest.Repo, Digest: p.Digest(), ConfigJSON: string(b), Reason: "Reviewed fixture exclusions", AllowExclusions: true, AllowSuppressions: true, AllowAnnotation: true}))
}
func runtimeContext(t *testing.T, f runtimeFixture) context.Context {
	t.Helper()
	ctx, err := execution.With(t.Context(), f.identity)
	require.NoError(t, err)
	return ctx
}
func TestRepositoryPolicyApprovalScope(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	source := "version: 1\nignore:\n  paths: [fixtures/**]\n"
	p := indexedPolicy(t, f, source)
	_, err := f.server.approveIngestPolicy(t.Context(), storetest.Repo, p)
	require.Error(t, err)
	approvePolicy(t, f, p)
	digest, err := f.server.approveIngestPolicy(t.Context(), storetest.Repo, p)
	require.NoError(t, err)
	assert.Equal(t, digest, p.Digest())
	other, org, err := f.accounts.SignUp(t.Context(), "policy-other@example.com", "Other", pw)
	require.NoError(t, err)
	require.NoError(t, f.accounts.St.AddRepository(t.Context(), accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, AddedBy: other.ID, AddedAt: time.Now()}))
	_, err = f.server.approveIngestPolicy(t.Context(), storetest.Repo, p)
	require.Error(t, err, "one tenant cannot change a shared graph's coverage")
	path := "/api/v1/repos/" + storetest.Repo + "/policy"
	in, _ := json.Marshal(repoPolicyRequest{Config: source, Digest: p.Digest(), Reason: "Reviewed", AllowExclusions: true})
	assert.Equal(t, f.client.do("PUT", path, string(in), nil), 403, "API keys cannot approve policy")
	browser := newClient(t, f.client.ts)
	require.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"runtime@example.com","password":"`+pw+`"}`, nil), 200)
	var preview struct{ Digest string }
	require.Equal(t, browser.do("POST", path+"/preview", string(in), &preview), 200)
	assert.Equal(t, preview.Digest, p.Digest())
	require.Equal(t, browser.do("PUT", path, string(in), nil), 200)
	require.Equal(t, browser.do("DELETE", path, "", nil), 204)
	_, err = f.server.approveIngestPolicy(t.Context(), storetest.Repo, p)
	require.Error(t, err)
}
func TestRepositorySuppressionIsTenantLocal(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	source := "version: 1\nscanners:\n  security:\n    ignore_rules:\n      - rule_id: yaml-hardcoded-secret\n        paths: [test.yaml]\n"
	p := indexedPolicy(t, f, source)
	approvePolicy(t, f, p)
	ctx := context.WithValue(t.Context(), scopeKey{}, scope{reach: map[string]accounts.Reach{storetest.Repo: {OrgID: f.org.ID}}})
	opts := f.server.repositorySecurityOptions(ctx, storetest.Repo, "")
	require.True(t, opts.Suppression != nil)
	assert.Equal(t, opts.Suppression(model.Node{Repo: storetest.Repo, Rule: "yaml-hardcoded-secret", File: "test.yaml"}), "Reviewed fixture exclusions")
	assert.Equal(t, opts.Suppression(model.Node{Repo: storetest.Repo, Rule: "yaml-hardcoded-secret", File: "production.yaml"}), "")
	other := context.WithValue(t.Context(), scopeKey{}, scope{reach: map[string]accounts.Reach{storetest.Repo: {OrgID: "foreign"}}})
	assert.True(t, f.server.repositorySecurityOptions(other, storetest.Repo, "").Suppression == nil)
	require.NoError(t, f.accounts.DeleteRepoPolicy(t.Context(), accounts.Principal{User: f.user, Via: "session"}, f.org.ID, storetest.Repo))
	assert.True(t, f.server.repositorySecurityOptions(ctx, storetest.Repo, "").Suppression == nil)
	var _ = opts
}
func TestAnnotationEndpointsAndStaleResults(t *testing.T) {
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct{ Role, Content string }
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		var batch annotation.Batch
		for _, message := range request.Messages {
			if message.Role == "user" {
				require.NoError(t, json.Unmarshal([]byte(message.Content), &batch))
			}
		}
		items := []annotation.Item{}
		for _, symbol := range batch.Symbols {
			items = append(items, annotation.Item{SymbolID: symbol.ID, Summary: "Indexed billing helper", Taxonomy: []string{"billing"}})
		}
		answer, err := json.Marshal(map[string]any{"items": items})
		require.NoError(t, err)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(answer)}}}})
	}))
	t.Cleanup(modelServer.Close)
	reg := llm.NewRegistry(llm.Config{Default: "local", Providers: []llm.ProviderConfig{{Name: "local", Kind: llm.KindOpenAI, BaseURL: modelServer.URL + "/v1", Model: "qwen", MaxInputTokens: 30000, MaxOutputTokens: 8000, TimeoutSeconds: 10}}})
	f := newRuntimeFixture(t, Config{LLM: reg})
	p := indexedPolicy(t, f, annotationConfig)
	approvePolicy(t, f, p)
	path := "/api/v1/repos/" + storetest.Repo + "/annotations"
	assert.Equal(t, f.client.do("POST", path, "{}", nil), 403)
	var submitted annotationSubmission
	var body string
	status := f.client.do("POST", path, "{}", &body, runtimeHeaders("thread-a")...)
	require.Equal(t, status, 202, body)
	require.NoError(t, json.Unmarshal([]byte(body), &submitted))
	require.True(t, len(submitted.Jobs) > 0)
	job := submitted.Jobs[0]
	deadline := time.Now().Add(5 * time.Second)
	for job.State != annotation.Succeeded && job.State != annotation.Failed && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		require.Equal(t, f.client.do("GET", path+"/jobs/"+job.ID, "", &job, runtimeHeaders("thread-a")...), 200)
	}
	require.Equal(t, job.State, annotation.Succeeded, job.Error)
	var result struct {
		Annotations []accounts.RepositoryAnnotation
	}
	require.Equal(t, f.client.do("GET", path, "", &result), 200)
	require.True(t, len(result.Annotations) > 0)
	assert.Equal(t, result.Annotations[0].OriginSessionID, "thread-a")
	require.Equal(t, f.client.do("POST", "/api/v1/execution/sessions", `{"session_id":"thread-b"}`, nil), 201)
	assert.Equal(t, f.client.do("GET", path+"/jobs/"+job.ID, "", nil, runtimeHeaders("thread-b")...), 404)
	g := storetest.Fixture()
	n, _ := g.Node(model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo))
	n.Time = "new-revision"
	require.NoError(t, f.store.ReplaceRepo(t.Context(), storetest.Repo, g))
	require.Equal(t, f.client.do("GET", path, "", &result), 200)
	assert.Len(t, result.Annotations, 0)
	b := annotation.Batch{Graph: storetest.Repo, Commit: job.Commit, Revision: job.Revision, PolicyDigest: job.PolicyDigest}
	require.Error(t, f.server.checkAnnotationBatch(runtimeContext(t, f), b), "a changed graph invalidates pending model output")
	assert.False(t, strings.Contains(body, "api_key"))
}
