package api

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// roleRoutes is every route registered through s.route before routes
// checked permissions, with the least role it needed then. It is frozen: a
// route added later is checked by TestEveryRouteHasAPermission only. The
// roles keep the names they had then (see frozenRole).
var roleRoutes = map[string]struct {
	kind       policyKind
	least      accounts.Role
	governance bool
}{
	"DELETE /api/v1/execution/sessions/{session}":                    {inOrg, "guest", false},
	"DELETE /api/v1/orgs/{org}":                                      {local, "", false},
	"DELETE /api/v1/repos/{repo}":                                    {onRepo, "admin", false},
	"DELETE /api/v1/repos/{repo}/annotations/jobs/{job}":             {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/architecture/links/{id}":            {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/images":                             {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/memory":                             {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/policy":                             {onRepo, "admin", false},
	"DELETE /api/v1/repos/{repo}/runtime":                            {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/sarif":                              {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/session-memory":                     {onRepo, "member", false},
	"DELETE /api/v1/repos/{repo}/triage/{fingerprint}":               {onRepo, "admin", false},
	"DELETE /api/v1/reviews/{id}":                                    {onReview, "admin", false},
	"DELETE /api/v1/watches/{host}/{login}":                          {inOrg, "member", false},
	"GET /api/v1/agent-rules":                                        {open, "", false},
	"GET /api/v1/agents/status":                                      {listed, "", false},
	"GET /api/v1/ai/providers":                                       {open, "", true},
	"GET /api/v1/ai/status":                                          {open, "", true},
	"GET /api/v1/attention":                                          {listed, "", false},
	"GET /api/v1/execution/sessions/{session}/activity":              {inOrg, "guest", false},
	"GET /api/v1/jobs":                                               {listed, "", false},
	"GET /api/v1/jobs/{id}":                                          {listed, "", false},
	"GET /api/v1/memory/jobs":                                        {listed, "", false},
	"GET /api/v1/memory/jobs/{id}":                                   {listed, "", false},
	"GET /api/v1/memory/status":                                      {open, "", false},
	"GET /api/v1/notifications":                                      {listed, "", false},
	"GET /api/v1/orgs":                                               {local, "", false},
	"GET /api/v1/overview":                                           {listed, "", false},
	"GET /api/v1/repos":                                              {listed, "", false},
	"GET /api/v1/repos/{repo}/annotations":                           {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/annotations/jobs/{job}":                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture":                          {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/context":                  {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/issues":                   {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/links":                    {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/proposals":                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/proposals/{id}":           {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/proposals/{id}/export":    {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/architecture/record":                   {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/branches":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/cleanup":                               {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/cleanup/plan":                          {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/code-records":                          {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/conventions":                           {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/dashboard":                             {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/dependencies":                          {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/duplicates":                            {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/duplicates/refactor":                   {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/entry-points":                          {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/graph":                                 {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/graph/expand":                          {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/graph/overview":                        {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/history":                               {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/hotspots":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/identities":                            {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/identities/reviews":                    {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/images":                                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/ingestion":                             {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/ingestion/{run}":                       {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/licenses":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/list":                                  {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/memory":                                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/metrics":                               {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/outline":                               {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/overview":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/packages/graph":                        {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/policy":                                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/reviews":                               {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/runtime":                               {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/sarif":                                 {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/sbom":                                  {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/schedule":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/search":                                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/sections":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/security":                              {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/session-memory":                        {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/symbol":                                {onRepo, "viewer", false},
	"GET /api/v1/repos/{repo}/triage":                                {onRepo, "viewer", false},
	"GET /api/v1/reviews/{id}":                                       {onReview, "viewer", false},
	"GET /api/v1/reviews/{id}/code":                                  {onReview, "viewer", false},
	"GET /api/v1/rules":                                              {open, "", true},
	"GET /api/v1/search":                                             {listed, "", false},
	"GET /api/v1/settings":                                           {open, "", true},
	"GET /api/v1/shared-code":                                        {listed, "", false},
	"GET /api/v1/sources":                                            {local, "", false},
	"GET /api/v1/stats":                                              {listed, "", false},
	"GET /api/v1/stats/models":                                       {open, "", true},
	"GET /api/v1/stats/queue":                                        {open, "", true},
	"GET /api/v1/stats/savings":                                      {listed, "", false},
	"GET /api/v1/stats/features":                                     {listed, "", false},
	"GET /api/v1/stats/series":                                       {open, "", true},
	"GET /api/v1/stats/series/batch":                                 {open, "", true},
	"GET /api/v1/version":                                            {open, "", true},
	"GET /api/v1/watches":                                            {inOrg, "viewer", false},
	"GET /healthz":                                                   {open, "", true},
	"PATCH /api/v1/reviews/{id}/comments/{comment}":                  {onReview, "member", false},
	"POST /api/v1/ai/providers/{name}/test":                          {platform, "", true},
	"POST /api/v1/execution/activity":                                {inOrg, "guest", false},
	"POST /api/v1/execution/sessions":                                {inOrg, "guest", false},
	"POST /api/v1/imports/list":                                      {inOrg, "member", false},
	"POST /api/v1/ingest":                                            {local, "", false},
	"POST /api/v1/notifications/seen":                                {listed, "", false},
	"POST /api/v1/orgs":                                              {local, "", false},
	"POST /api/v1/orgs/{org}/repos/{name}/index":                     {local, "", false},
	"POST /api/v1/orgs/{org}/scan":                                   {local, "", false},
	"POST /api/v1/repos":                                             {inOrg, "member", false},
	"POST /api/v1/repos/{repo}/ai/{task}":                            {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/annotations":                          {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/architecture/links":                   {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/architecture/proposals":               {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/architecture/proposals/{id}/withdraw": {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/ask-local":                            {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/brief-plan":                           {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/code-records":                         {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/graph/edges":                          {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/identities/reviews":                   {onRepo, "admin", false},
	"POST /api/v1/repos/{repo}/impact":                               {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/local-draft":                          {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/memory/index":                         {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/memory/offload":                       {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/memory/recall":                        {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/policy/preview":                       {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/remote":                               {onRepo, "admin", false},
	"POST /api/v1/repos/{repo}/semantic":                             {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/session-memory":                       {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/session-memory/offload":               {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/session-memory/promote":               {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/session-memory/recall":                {onRepo, "viewer", false},
	"POST /api/v1/repos/{repo}/source-extract":                       {onRepo, "member", false},
	"POST /api/v1/repos/{repo}/sync":                                 {onRepo, "admin", false},
	"POST /api/v1/repos/{repo}/usage/retrieval":                      {onRepo, "viewer", false},
	"POST /api/v1/reviews/{id}/fix":                                  {onReview, "member", false},
	"POST /api/v1/usage/local":                                       {listed, "", false},
	"POST /api/v1/usage/unindexed":                                   {open, "", false},
	"POST /api/v1/watches":                                           {inOrg, "member", false},
	"POST /api/v1/watches/{host}/{login}/scan":                       {inOrg, "member", false},
	"PUT /api/v1/ai/routing":                                         {platform, "", true},
	"PUT /api/v1/orgs/{org}":                                         {local, "", false},
	"PUT /api/v1/repos/{repo}/images":                                {onRepo, "member", false},
	"PUT /api/v1/repos/{repo}/policy":                                {onRepo, "admin", false},
	"PUT /api/v1/repos/{repo}/runtime":                               {onRepo, "member", false},
	"PUT /api/v1/repos/{repo}/sarif":                                 {onRepo, "member", false},
	"PUT /api/v1/repos/{repo}/schedule":                              {onRepo, "admin", false},
	"PUT /api/v1/repos/{repo}/triage":                                {onRepo, "admin", false},
	"PUT /api/v1/watches/{host}/{login}":                             {inOrg, "member", false},
}

var builtInRoles = []accounts.Role{accounts.Deny, accounts.Guest, accounts.Read, accounts.Write, accounts.Full, accounts.Owner}

// TestBuiltInRolesKeepAccess proves that checking permissions changed no
// one's access: for every route that existed when roles were checked, each
// built-in role holds the route's permission exactly when it was at least the
// role the route needed.
// frozenRole maps the role names in roleRoutes to today's.
var frozenRole = map[accounts.Role]accounts.Role{"guest": accounts.Guest, "viewer": accounts.Read, "member": accounts.Write, "admin": accounts.Full, "owner": accounts.Owner}

func TestBuiltInRolesKeepAccess(t *testing.T) {
	srv, _, _ := rootServer(t)
	for pattern, old := range roleRoutes {
		pol, ok := srv.policies[pattern]
		if !assert.True(t, ok, "route gone: "+pattern) {
			continue
		}
		assert.Equal(t, pol.kind, old.kind, pattern)
		assert.Equal(t, pol.governance, old.governance, pattern)
		if old.least == "" {
			assert.Equal(t, pol.perm, accounts.Permission(""), pattern)
			continue
		}
		for _, r := range builtInRoles {
			assert.Equal(t, r.Has(pol.perm), r.AtLeast(frozenRole[old.least]), pattern+" as "+string(r)+" ("+string(pol.perm)+")")
		}
	}
	// The gateway routes (registered only with the gateway on) needed Member.
	assert.Equal(t, accounts.OrgGatewayWrite.Floor(), accounts.Write)
}

// TestEveryRouteHasAPermission keeps every route that acts on a repository,
// a review or an organisation tied to a known permission of the right
// resource, and every other route free of one.
func TestEveryRouteHasAPermission(t *testing.T) {
	srv, _, _ := rootServer(t)
	require.True(t, len(srv.policies) > 100, "routes record their policies")
	for pattern, pol := range srv.policies {
		switch pol.kind {
		case onRepo, onReview:
			assert.True(t, pol.perm.Valid() && pol.perm.Resource() == accounts.ResourceRepository, pattern+": "+string(pol.perm))
		case inOrg:
			assert.True(t, pol.perm.Valid() && pol.perm.Resource() == accounts.ResourceOrganisation, pattern+": "+string(pol.perm))
		default:
			assert.Equal(t, pol.perm, accounts.Permission(""), pattern)
		}
	}
}
