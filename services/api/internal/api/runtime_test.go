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

// Reports of the fixture repository: Handler (app/app.go:8-20) half
// covered, TestHandler (app/app_test.go) retried after a failure.
const (
	fixtureCoverage = "mode: set\nexample.com/app/app/app.go:9.2,12.3 2 1\nexample.com/app/app/app.go:13.2,19.3 2 0\nexample.com/other/x.go:1.1,2.2 1 1\n"
	fixtureJUnit    = `<testsuites><testsuite><testcase classname="example.com/app/app" name="TestHandler"><failure/></testcase><testcase classname="example.com/app/app" name="TestHandler"/></testsuite></testsuites>`
)

func runtimeBody(t *testing.T, commit string) string {
	t.Helper()
	b, err := json.Marshal(runtimeUpload{Commit: commit, Reports: []runtimeReport{
		{Kind: "coverage", Name: "ci/cover.out", Content: fixtureCoverage},
		{Kind: "junit", Name: "junit.xml", Content: fixtureJUnit},
	}})
	require.NoError(t, err)
	return string(b)
}

func TestRuntimeUpload(t *testing.T) {
	ts, _ := newTestServer(t)
	base := "/api/v1/repos/" + storetest.Repo + "/runtime"
	send := func(body, encoding string) (int, RuntimeSummary) {
		t.Helper()
		var rd io.Reader = strings.NewReader(body)
		if encoding == "gzip" {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write([]byte(body))
			require.NoError(t, zw.Close())
			rd = &buf
		}
		req, err := http.NewRequest(http.MethodPut, ts.URL+base, rd) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close() //nolint:errcheck // test
		var sum RuntimeSummary
		if res.StatusCode == 200 {
			require.NoError(t, json.NewDecoder(res.Body).Decode(&sum))
		}
		return res.StatusCode, sum
	}

	assert.Equal(t, get(t, ts, base, nil), 404, "nothing recorded yet")
	code, sum := send(runtimeBody(t, "DEF5678"), "gzip")
	require.Equal(t, code, 200)
	assert.Equal(t, sum.Commit, "def5678")
	assert.Equal(t, sum.Origin, "upload")
	assert.Equal(t, sum.Sources, []string{"coverage cover.out", "junit junit.xml"}, "names lose their folders")
	assert.Equal(t, sum.Functions, 1)
	assert.Equal(t, sum.UnmatchedFiles, 1)
	assert.Equal(t, sum.Flaky, []string{"app.TestHandler"})
	assert.True(t, sum.Stale, "the fixture graph indexed another commit (abc)")

	var got RuntimeSummary
	require.Equal(t, get(t, ts, base, &got), 200)
	assert.Equal(t, got.Commit, "def5678")

	tests := []struct{ name, body, encoding string }{
		{"no commit", `{"commit":"","reports":[{"kind":"junit","name":"j.xml","content":"<testsuite/>"}]}`, ""},
		{"not hex", `{"commit":"main","reports":[{"kind":"junit","name":"j.xml","content":"<testsuite/>"}]}`, ""},
		{"no reports", `{"commit":"abc1234","reports":[]}`, ""},
		{"kind", `{"commit":"abc1234","reports":[{"kind":"sarif","name":"x","content":""}]}`, ""},
		{"name", `{"commit":"abc1234","reports":[{"kind":"junit","name":"a b","content":"<testsuite/>"}]}`, ""},
		{"bad report", `{"commit":"abc1234","reports":[{"kind":"coverage","name":"c.out","content":"mode: set\nbroken"}]}`, ""},
		{"unknown field", `{"commit":"abc1234","reports":[],"x":1}`, ""},
		{"encoding", runtimeBody(t, "abc1234"), "br"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _ := send(tt.body, tt.encoding)
			assert.Equal(t, code, 400)
		})
	}

	req, err := http.NewRequest(http.MethodDelete, ts.URL+base, nil) //nolint:noctx // test
	require.NoError(t, err)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	res.Body.Close() //nolint:errcheck,gosec // test
	assert.Equal(t, res.StatusCode, 204)
	assert.Equal(t, get(t, ts, base, nil), 404)
}

// TestRuntimeUploadIsPerOrganisation: two organisations share the graph; an
// upload by one never reaches the other, and viewers cannot upload.
func TestRuntimeUploadIsPerOrganisation(t *testing.T) {
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
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 201)
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
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/members", `{"email":"carol@example.com","role":"guest"}`, nil), 200)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+acme.Slug+"/repos/"+url.PathEscape(storetest.Repo)+"/access", `{"user":"carol@example.com","role":"viewer"}`, nil), 200)
	inAcme, inExt := []string{OrgHeader, acme.Slug}, []string{OrgHeader, ext.Slug}
	base := "/api/v1/repos/" + url.PathEscape(storetest.Repo) + "/runtime"

	assert.Equal(t, carol.do("PUT", base, runtimeBody(t, "abc1234"), nil, inAcme...), 403, "a viewer cannot upload")
	require.Equal(t, bob.do("PUT", base, runtimeBody(t, "abc1234"), nil, inAcme...), 200)
	var sum RuntimeSummary
	require.Equal(t, carol.do("GET", base, "", &sum, inAcme...), 200, "a viewer reads it")
	assert.Equal(t, sum.Commit, "abc1234")
	assert.Equal(t, dave.do("GET", base, "", nil, inExt...), 404, "the other organisation has none")

	f, err := st.RuntimeFacts(ctx, agentmem.Namespace(acme.ID, storetest.Repo))
	require.NoError(t, err)
	assert.Equal(t, f.Commit, "abc1234", "kept in the uploading organisation's namespace")
}

// TestRuntimeHistory: a test that failed and then passed at the same
// commit (a CI rerun) is flaky across runs, though neither upload saw it
// pass and fail.
func TestRuntimeHistory(t *testing.T) {
	ts, _ := newTestServer(t)
	base := "/api/v1/repos/" + storetest.Repo + "/runtime"
	upload := func(commit string, failed bool) RuntimeSummary {
		t.Helper()
		tc := `<testcase classname="example.com/app/app" name="TestHandler"/>`
		if failed {
			tc = `<testcase classname="example.com/app/app" name="TestHandler"><failure/></testcase>`
		}
		b, err := json.Marshal(runtimeUpload{Commit: commit, Reports: []runtimeReport{{Kind: "junit", Name: "junit.xml", Content: "<testsuite>" + tc + "</testsuite>"}}})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPut, ts.URL+base, bytes.NewReader(b)) //nolint:noctx // test
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close() //nolint:errcheck // test
		require.Equal(t, res.StatusCode, 200)
		var sum RuntimeSummary
		require.NoError(t, json.NewDecoder(res.Body).Decode(&sum))
		return sum
	}
	sum := upload("aaa1111", true)
	assert.Equal(t, sum.Runs, 1)
	assert.Equal(t, sum.Failed, 1)
	assert.Equal(t, sum.FlakyAcrossRuns, []string{})
	sum = upload("aaa1111", false)
	assert.Equal(t, sum.Runs, 2)
	assert.Equal(t, sum.Flaky, []string{}, "each run alone was clean")
	assert.Equal(t, sum.FlakyAcrossRuns, []string{"app.TestHandler"})
	sum = upload("bbb2222", false)
	assert.Equal(t, sum.FlakyAcrossRuns, []string{"app.TestHandler"}, "still within the recorded runs")
}
