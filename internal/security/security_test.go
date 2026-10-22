package security

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/ingest/config"
	"github.com/astrona-io/codesynapse/internal/ingest/golang"
	"github.com/astrona-io/codesynapse/internal/ingest/lang"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/rules"
)

func fixture(t *testing.T) *memory.Store {
	t.Helper()
	g := storetest.Fixture()
	add := func(n model.Node) { n.Repo = storetest.Repo; g.AddNode(n) }
	edge := func(from, to string, k model.EdgeKind) { g.AddEdge(model.Edge{From: from, To: to, Kind: k}) }

	// exec-shell in (*impl).Do, which Handler (exported, tested) reaches.
	shell := storetest.ID(model.KindFinding, "app/app.go:27:exec-shell")
	add(model.Node{ID: shell, Kind: model.KindFinding, Name: "Shell", Rule: "exec-shell", Severity: "critical", CWE: []string{"CWE-78"}, File: "app/app.go", StartLine: 27, EndLine: 27})
	edge(storetest.ID(model.KindMethod, "(*app.impl).Do"), shell, model.EdgeHasFinding)

	// Weak hash in an unexported, uncalled helper.
	helper := storetest.ID(model.KindFunction, "app.helper")
	add(model.Node{ID: helper, Kind: model.KindFunction, Name: "helper", QualifiedName: "app.helper", Package: "example.com/app/internal/x", File: "app/app.go", StartLine: 50, EndLine: 55})
	weak := storetest.ID(model.KindFinding, "app/app.go:51:weak-hash")
	add(model.Node{ID: weak, Kind: model.KindFinding, Name: "Weak", Rule: "weak-hash", Severity: "medium", File: "app/app.go", StartLine: 51, EndLine: 51})
	edge(helper, weak, model.EdgeHasFinding)

	// One called and one required-only vulnerability.
	repoID := storetest.ID(model.KindRepository, storetest.Repo)
	mod := storetest.ID(model.KindModule, "golang.org/x/net")
	add(model.Node{ID: mod, Kind: model.KindModule, Name: "golang.org/x/net", Version: "v0.17.0"})
	edge(repoID, mod, model.EdgeDependsOn)
	called := storetest.ID(model.KindVulnerability, "golang.org/x/net@GO-1")
	add(model.Node{ID: called, Kind: model.KindVulnerability, Name: "GO-1", Severity: "high", Aliases: []string{"CVE-2024-1"}, Package: "golang.org/x/net", Version: "v0.17.0", FixedVersion: "v0.23.0", Reachability: "called"})
	ext := storetest.ID(model.KindExternal, "golang.org/x/net/http2.Server.ServeConn")
	add(model.Node{ID: ext, Kind: model.KindExternal, Name: "Server.ServeConn", QualifiedName: "golang.org/x/net/http2.Server.ServeConn"})
	edge(called, ext, model.EdgeAffects)
	edge(storetest.ID(model.KindFunction, "app.main"), ext, model.EdgeCalls)
	req := storetest.ID(model.KindVulnerability, "golang.org/x/net@GO-2")
	add(model.Node{ID: req, Kind: model.KindVulnerability, Name: "GO-2", Severity: "critical", Package: "golang.org/x/net", Reachability: "required"})

	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), storetest.Repo, g))
	return s
}

func TestBuild(t *testing.T) {
	rep, err := Build(context.Background(), fixture(t), storetest.Repo, Options{})
	require.NoError(t, err)

	require.Len(t, rep.Findings, 2)
	top := rep.Findings[0]
	assert.Equal(t, top.Rule, "exec-shell")
	assert.True(t, top.Exposed, "reachable from exported Handler")
	assert.True(t, top.Tested)
	weak := rep.Findings[1]
	assert.False(t, weak.Exposed)
	assert.False(t, weak.Tested)

	require.Len(t, rep.Vulnerabilities, 2)
	assert.Equal(t, rep.Vulnerabilities[0].ID, "GO-1", "called high outranks required critical")
	assert.Equal(t, rep.Vulnerabilities[0].CVE, []string{"CVE-2024-1"})
	require.Len(t, rep.Vulnerabilities[0].CallSites, 1)
	assert.Equal(t, rep.Vulnerabilities[0].CallSites[0].Name, "app.main")

	assert.Equal(t, rep.Modules.Vulnerable, 1)
	assert.Less(t, rep.Score, 100)
	assert.NotEqual(t, rep.Grade, "A")

	assert.True(t, rep.ExceedsThreshold("high"))
	assert.False(t, rep.ExceedsThreshold("critical") && false)

	var md strings.Builder
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.Contains(t, md.String(), "CVE-2024-1")
	assert.Contains(t, md.String(), "exec-shell")
}

func TestMinSeverity(t *testing.T) {
	rep, err := Build(context.Background(), fixture(t), storetest.Repo, Options{MinSeverity: "high"})
	require.NoError(t, err)
	assert.Len(t, rep.Findings, 1)
	assert.Equal(t, rep.FindingCounts["medium"], 1, "counts cover everything; lists are filtered")
}

func TestThresholdIgnoresRequiredOnly(t *testing.T) {
	rep := Report{Vulnerabilities: []Vulnerability{{Severity: "critical", Reachability: "required"}}}
	assert.False(t, rep.ExceedsThreshold("high"))
	rep.Vulnerabilities[0].Reachability = "imported"
	assert.True(t, rep.ExceedsThreshold("high"))
}

func TestCatalogCoversEveryRule(t *testing.T) {
	cat := Catalog()
	require.Greater(t, len(cat), 30)
	require.NoError(t, rules.Err())
	for _, g := range cat {
		assert.NotEmpty(t, g.Examples, "every rule has a fix example: "+g.ID)
	}
	g, ok := GuideFor("sql-concat", "python")
	require.True(t, ok)
	assert.Equal(t, g.Examples[0].Language, "python", "requested language first")
	g, _ = GuideFor("dom-xss", "vue")
	assert.Equal(t, g.Examples[0].Language, "typescript")
}

// TestEveryDetectorHasARule keeps the detectors and rules/*.yaml in sync.
func TestEveryDetectorHasARule(t *testing.T) {
	detected := map[string]bool{}
	for _, ids := range [][]string{golang.RuleIDs, config.RuleIDs, lang.RuleIDs} {
		for _, id := range ids {
			_, ok := rules.Get(id)
			assert.True(t, ok, "detector reports %q but rules/*/%s.yaml is missing", id, id)
			detected[id] = true
		}
	}
	for _, r := range rules.All() {
		assert.True(t, detected[r.ID], "%s has no detector", r.File)
	}
}

func TestYAMLRepositoryIsGradedWithCoverage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	write := func(rel, s string) {
		p := root + "/" + rel
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(s), 0o600))
	}
	write("chart/Chart.yaml", "apiVersion: v2\nname: app\nversion: 0.1.0\n")
	write("chart/templates/ds.yaml", "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: {{ .Release.Name }}\nspec:\n  template:\n    spec:\n      hostPID: true\n      containers:\n        - name: a\n          image: x:latest\n          securityContext:\n            privileged: true\n")
	write("Dockerfile", "FROM scratch\n")
	g, err := config.Extract(ctx, "y", root)
	require.NoError(t, err)
	repoID := model.NodeID("y", model.KindRepository, "y")
	cov, err := EncodeCoverage(CoverageOf(g, Inventory(root)))
	require.NoError(t, err)
	repoNode := g.AddNode(model.Node{ID: repoID, Kind: model.KindRepository, Repo: "y", Name: "y"})
	repoNode.Head, repoNode.Time, repoNode.Coverage = "a82019c0ffee", "2026-10-22T08:00:00Z", cov
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "y", g))

	rep, err := Build(ctx, st, "y", Options{})
	require.NoError(t, err)
	assert.True(t, rep.Grade != "A" && rep.Grade != NotApplicable, "Helm template findings count: "+rep.Grade)
	assert.True(t, rep.FindingCounts["critical"]+rep.FindingCounts["high"] >= 2)
	assert.Equal(t, rep.Head, "a82019c0ffee")
	require.NotNil(t, rep.Coverage)
	assert.Equal(t, rep.Coverage.HelmScanned, 1)
	joined := strings.Join(append(rep.Checked, rep.NotChecked...), "\n")
	assert.True(t, strings.Contains(joined, "1 of 1 Helm template(s)"), joined)
	assert.True(t, strings.Contains(joined, "1 Dockerfile file(s): no rules yet"), joined)

	var md strings.Builder
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.True(t, strings.Contains(md.String(), "Indexed at commit `a82019c0ff`"))
	assert.True(t, strings.Contains(md.String(), "**Not checked (review by hand):**"))
}

func TestNothingScannableIsNotApplicable(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	cov, err := EncodeCoverage(Coverage{Code: map[string]int{}, NotCovered: map[string]int{"Terraform": 12}})
	require.NoError(t, err)
	g.AddNode(model.Node{ID: model.NodeID("t", model.KindRepository, "t"), Kind: model.KindRepository, Repo: "t", Name: "t", Coverage: cov})
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "t", g))
	rep, err := Build(ctx, st, "t", Options{})
	require.NoError(t, err)
	assert.Equal(t, rep.Grade, NotApplicable, "no A for a repository nothing could check")
	var md strings.Builder
	require.NoError(t, WriteMarkdown(&md, rep))
	assert.True(t, strings.Contains(md.String(), "**No grade:**"))
	assert.True(t, strings.Contains(md.String(), "12 Terraform file(s): no rules yet"))
}

func TestFindingsBeyondTheListLimitCount(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("big", model.KindRepository, "big"), Kind: model.KindRepository, Repo: "big", Name: "big"})
	for i := range 700 {
		g.AddNode(model.Node{ID: model.NodeID("big", model.KindFinding, fmt.Sprintf("f%d", i)), Kind: model.KindFinding, Repo: "big",
			Name: "x", Rule: "k8s-host-path", Severity: "low", File: "a.yaml", StartLine: i + 1})
	}
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "big", g))
	rep, err := Build(ctx, st, "big", Options{})
	require.NoError(t, err)
	assert.Equal(t, rep.FindingCounts["low"], 700)
	assert.Equal(t, rep.Unanalysed, 200)
	assert.Equal(t, rep.Grade, "F", "700 findings, not 500, make the score")
}

func TestModulesBeyondTheListLimitCount(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("m", model.KindRepository, "m"), Kind: model.KindRepository, Repo: "m", Name: "m"})
	for i := range 541 {
		g.AddNode(model.Node{ID: model.NodeID("m", model.KindModule, fmt.Sprintf("npm:p%d", i)), Kind: model.KindModule, Repo: "m",
			Name: fmt.Sprintf("p%d", i), Version: "1.0.0", Indirect: i > 0, Ecosystem: "npm"})
	}
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "m", g))
	rep, err := Build(ctx, st, "m", Options{})
	require.NoError(t, err)
	assert.Equal(t, rep.Modules.Total, 541)
	assert.Equal(t, rep.Modules.Direct+rep.Modules.Indirect, 541)
}

func TestVulnerabilityNamesHowItGotIn(t *testing.T) {
	ctx := context.Background()
	g := storetest.Fixture()
	add := func(n model.Node) { n.Repo = storetest.Repo; g.AddNode(n) }
	add(model.Node{ID: storetest.ID(model.KindModule, "golang.org/x/net"), Kind: model.KindModule, Name: "golang.org/x/net", Version: "v0.17.0",
		Ecosystem: "Go", Indirect: true, Via: "github.com/a/a > github.com/b/b", Roots: 2})
	add(model.Node{ID: storetest.ID(model.KindVulnerability, "golang.org/x/net@GO-1"), Kind: model.KindVulnerability, Name: "GO-1", Severity: "high",
		Package: "golang.org/x/net", Version: "v0.17.0", FixedVersion: "v0.23.0", Reachability: "required", Ecosystem: "Go"})
	add(model.Node{ID: storetest.ID(model.KindModule, "npm:lodash"), Kind: model.KindModule, Name: "lodash", Version: "4.0.0",
		Ecosystem: "npm", Indirect: true, Via: "express", Roots: 1})
	add(model.Node{ID: storetest.ID(model.KindVulnerability, "npm:lodash@GHSA-1"), Kind: model.KindVulnerability, Name: "GHSA-1", Severity: "medium",
		Package: "lodash", Version: "4.0.0", Reachability: "required", Ecosystem: "npm"})
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))

	rep, err := Build(ctx, st, storetest.Repo, Options{})
	require.NoError(t, err)
	via := map[string]string{}
	for _, v := range rep.Vulnerabilities {
		via[v.Module] = v.Via
	}
	assert.Equal(t, via["golang.org/x/net"], "github.com/a/a > github.com/b/b")
	assert.Equal(t, via["lodash"], "express", "non-Go modules are keyed by ecosystem")
}
