package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestImageScanIsPerOrganisation(t *testing.T) {
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
	base := "/api/v1/repos/" + url.PathEscape(storetest.Repo) + "/images"
	report := `{"ArtifactName":"app:1","Results":[{"Vulnerabilities":[
{"VulnerabilityID":"CVE-2025-0001","PkgName":"libssl3","InstalledVersion":"3.0.1","FixedVersion":"3.0.2","Severity":"HIGH"},
{"VulnerabilityID":"CVE-2025-0002","PkgName":"zlib","InstalledVersion":"1.2","Severity":"CRITICAL"}]}]}`

	assert.Equal(t, carol.do("PUT", base, report, nil, inAcme...), 403, "a viewer cannot upload")
	assert.Equal(t, bob.do("PUT", base, "not json", nil, inAcme...), 400)
	var sum ImageScanSummary
	require.Equal(t, bob.do("PUT", base, report, &sum, inAcme...), 200)
	assert.Equal(t, sum.Total, 2)
	assert.Equal(t, sum.Findings[0].ID, "CVE-2025-0002", "worst first")
	var again ImageScanSummary
	require.Equal(t, carol.do("GET", base, "", &again, inAcme...), 200, "a viewer reads it")
	assert.Equal(t, again.Counts["critical"], 1)
	assert.Equal(t, dave.do("GET", base, "", nil, inExt...), 404, "the other organisation has none")

	var sec struct {
		ContainerImages *ImageScanSummary `json:"container_images"`
	}
	require.Equal(t, carol.do("GET", "/api/v1/repos/"+url.PathEscape(storetest.Repo)+"/security", "", &sec, inAcme...), 200)
	require.NotNil(t, sec.ContainerImages)
	assert.Equal(t, sec.ContainerImages.Total, 2)
	assert.Equal(t, len(sec.ContainerImages.Findings), 0, "the report carries counts only")

	assert.Equal(t, carol.do("DELETE", base, "", nil, inAcme...), 403)
	require.Equal(t, bob.do("DELETE", base, "", nil, inAcme...), 204)
	assert.Equal(t, bob.do("GET", base, "", nil, inAcme...), 404)
}
