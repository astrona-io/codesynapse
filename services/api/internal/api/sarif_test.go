package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// sarifLog is a log of one tool: a finding in the fixture's app/app.go at
// an absolute CI path, one relative, and one with a secret in its message.
func sarifLog(tool, version string) string {
	return `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"` + tool + `","version":"` + version + `",
	  "rules":[{"id":"G101","helpUri":"https://example.com/rules/G101","defaultConfiguration":{"level":"error"}}]}},
	  "results":[
	    {"ruleId":"G101","message":{"text":"hardcoded credential password=hunter2hunter2"},
	     "locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///home/runner/work/app/app/app/app.go"},"region":{"startLine":9,"endLine":12,
	       "snippet":{"text":"password := \"hunter2hunter2\""}}}}]},
	    {"ruleId":"G101","level":"note","message":{"text":"weak"},
	     "locations":[{"physicalLocation":{"artifactLocation":{"uri":"deploy/values.yaml"},"region":{"startLine":3}}}]},
	    {"ruleId":"G101","message":{"text":"gone"},
	     "locations":[{"physicalLocation":{"artifactLocation":{"uri":"file:///tmp/other/nowhere.go"}}}]}
	  ]}]}`
}

func sendSARIF(t *testing.T, ts *httptest.Server, method, path, body, encoding string) (int, SARIFReport, string) {
	t.Helper()
	var rd io.Reader = strings.NewReader(body)
	if encoding == "gzip" {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(body))
		require.NoError(t, zw.Close())
		rd = &buf
	}
	req, err := http.NewRequest(method, ts.URL+path, rd) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/sarif+json")
	if encoding != "" {
		req.Header.Set("Content-Encoding", encoding)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var rep SARIFReport
	if res.StatusCode == 200 {
		require.NoError(t, json.Unmarshal(raw, &rep))
	}
	return res.StatusCode, rep, string(raw)
}

func TestSARIFUpload(t *testing.T) {
	ts, _ := newTestServer(t)
	base := "/api/v1/repos/" + storetest.Repo + "/sarif"

	var empty SARIFReport
	require.Equal(t, get(t, ts, base, &empty), 200)
	assert.Equal(t, len(empty.Tools), 0)

	code, rep, raw := sendSARIF(t, ts, http.MethodPut, base+"?commit=DEF5678", sarifLog("gosec", "2.21.4"), "gzip")
	require.Equal(t, code, 200, raw)
	require.Equal(t, len(rep.Tools), 1)
	tool := rep.Tools[0]
	assert.Equal(t, tool.Tool, "gosec")
	assert.Equal(t, tool.Version, "2.21.4")
	assert.Equal(t, tool.Commit, "def5678")
	assert.Equal(t, tool.Findings, 2)
	assert.Equal(t, tool.Unmatched, 1, "an absolute path no indexed file matches")
	assert.Equal(t, tool.BySeverity["high"], 1)
	assert.Equal(t, tool.BySeverity["low"], 1)
	assert.True(t, tool.Stale, "the fixture graph indexed another commit")
	assert.False(t, strings.Contains(raw, "hunter2"), "no secret and no snippet in the answer")

	code, _, raw = sendSARIF(t, ts, http.MethodPut, base+"?commit=abc1234", sarifLog("zizmor", ""), "")
	require.Equal(t, code, 200, raw)

	var got SARIFReport
	require.Equal(t, get(t, ts, base, &got), 200)
	assert.Equal(t, len(got.Tools), 2, "another tool's upload keeps the first")
	assert.Equal(t, got.Total, 4)
	require.Equal(t, len(got.Findings), 4)
	top := got.Findings[0]
	assert.Equal(t, top.Severity, "high")
	assert.Equal(t, top.Tool, "gosec")
	assert.Equal(t, top.FoundBy, "gosec 2.21.4")
	assert.Equal(t, top.File, "app/app.go", "placed on the indexed file")
	assert.Equal(t, top.StartLine, 9)
	assert.Equal(t, top.HelpURI, "https://example.com/rules/G101")
	assert.Equal(t, got.Findings[1].FoundBy, "zizmor")

	require.Equal(t, get(t, ts, base+"?tool=ZIZMOR&min_severity=high", &got), 200)
	assert.Equal(t, got.Total, 1)
	assert.Equal(t, get(t, ts, base+"?min_severity=bad", nil), 400)

	tests := []struct{ name, query, body, encoding string }{
		{"no commit", "", sarifLog("x", ""), ""},
		{"not hex", "?commit=main", sarifLog("x", ""), ""},
		{"not sarif", "?commit=abc1234", `{"version":"2.0.0","runs":[]}`, ""},
		{"no tool", "?commit=abc1234", sarifLog("", ""), ""},
		{"encoding", "?commit=abc1234", sarifLog("x", ""), "br"},
		{"bad gzip", "?commit=abc1234", sarifLog("x", ""), "gzip-bad"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc := tt.encoding
			if enc == "gzip-bad" {
				code := sendSARIFRaw(t, ts, base+tt.query, tt.body, "gzip")
				assert.Equal(t, code, 400)
				return
			}
			code, _, _ := sendSARIF(t, ts, http.MethodPut, base+tt.query, tt.body, enc)
			assert.Equal(t, code, 400)
		})
	}

	code, _, _ = sendSARIF(t, ts, http.MethodDelete, base+"?tool=gosec", "", "")
	assert.Equal(t, code, 204)
	require.Equal(t, get(t, ts, base, &got), 200)
	require.Equal(t, len(got.Tools), 1)
	assert.Equal(t, got.Tools[0].Tool, "zizmor")
	code, _, _ = sendSARIF(t, ts, http.MethodDelete, base+"?tool=gosec", "", "")
	assert.Equal(t, code, 404)
	code, _, _ = sendSARIF(t, ts, http.MethodDelete, base, "", "")
	assert.Equal(t, code, 204)
	require.Equal(t, get(t, ts, base, &got), 200)
	assert.Equal(t, len(got.Tools), 0)
}

// sendSARIFRaw sends body as is, claiming the encoding.
func sendSARIFRaw(t *testing.T, ts *httptest.Server, path, body, encoding string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, ts.URL+path, strings.NewReader(body)) //nolint:noctx // test
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", encoding)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	return res.StatusCode
}

// TestSARIFUploadIsPerOrganisation: two organisations share the graph; an
// upload by one never reaches the other, and viewers cannot upload.
func TestSARIFUploadIsPerOrganisation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 202)
		return c
	}
	_ = signup("ada@example.com") // platform admin
	bob, carol, dave := signup("bob@example.com"), signup("carol@example.com"), signup("dave@example.com")
	var acme, ext accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Ext"}`, &ext), 201)
	for _, link := range []struct{ org, email string }{{acme.ID, "bob@example.com"}, {ext.ID, "dave@example.com"}} {
		u, err := acc.St.UserByEmail(ctx, link.email)
		require.NoError(t, err)
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: link.org, Graph: storetest.Repo, AddedBy: u.ID, AddedAt: time.Now()}))
	}
	invite(t, bob, acme.Slug, carol, "carol@example.com", "guest")
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/repos/"+url.PathEscape(storetest.Repo)+"/access", `{"user":"carol@example.com","role":"viewer"}`, nil), 200)
	inAcme, inExt := []string{OrgHeader, acme.Slug}, []string{OrgHeader, ext.Slug}
	base := "/api/v1/repos/" + url.PathEscape(storetest.Repo) + "/sarif"
	put := base + "?commit=abc1234"

	assert.Equal(t, carol.do("PUT", put, sarifLog("gosec", "2"), nil, inAcme...), 403, "a viewer cannot upload")
	assert.Equal(t, carol.do("DELETE", base, "", nil, inAcme...), 403, "a viewer cannot delete")
	require.Equal(t, bob.do("PUT", put, sarifLog("gosec", "2"), nil, inAcme...), 200)
	var rep SARIFReport
	require.Equal(t, carol.do("GET", base, "", &rep, inAcme...), 200, "a viewer reads it")
	assert.Equal(t, rep.Total, 2)
	require.Equal(t, dave.do("GET", base, "", &rep, inExt...), 200)
	assert.Equal(t, rep.Total, 0, "the other organisation has none")

	set, err := st.SARIFFindings(ctx, agentmem.Namespace(acme.ID, storetest.Repo))
	require.NoError(t, err)
	require.Equal(t, len(set.Runs), 1, "kept in the uploading organisation's namespace")
	assert.Equal(t, set.Runs[0].Tool, "gosec")
}
