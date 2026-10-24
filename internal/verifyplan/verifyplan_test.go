package verifyplan

import (
	"context"
	"math"
	"testing"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/review"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestBuild(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	fn := func(local, name, file string, line int, test bool) string {
		id := model.NodeID("r", model.KindFunction, local)
		g.AddNode(model.Node{ID: id, Kind: model.KindFunction, Repo: "r", Name: name, QualifiedName: local, File: file, StartLine: line, EndLine: line + 2, IsTest: test})
		return id
	}
	calls := func(from, to string) { g.AddEdge(model.Edge{From: from, To: to, Kind: model.EdgeCalls}) }
	f := fn("example.com/a.F", "F", "a/a.go", 1, false)
	fn("example.com/a.U", "U", "a/a.go", 10, false)
	h := fn("example.com/b.H", "H", "b/b.go", 1, false)
	calls(fn("example.com/a.TestF", "TestF", "a/a_test.go", 1, true), f)
	calls(fn("example.com/a.helper", "helper", "a/a_test.go", 10, true), f)
	calls(h, f)
	calls(fn("example.com/b.TestG", "TestG", "b/b_test.go", 1, true), h)
	p := fn("pkg.py::p", "p", "pkg.py", 1, false)
	calls(fn("tests/test_pkg.py::TestP.test_it", "test_it", "tests/test_pkg.py", 3, true), p)
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, "r", g))

	whole := []gitx.LineRange{{Start: 1, End: math.MaxInt32}}
	rep, err := review.Analyze(ctx, query.Service{R: mem}, "r", []gitx.FileChange{{Path: "a/a.go", Ranges: whole}, {Path: "pkg.py", Ranges: whole}}, review.Options{Depth: 3})
	require.NoError(t, err)
	plan, err := Build(ctx, mem, "r", rep)
	require.NoError(t, err)

	var commands []string
	for _, s := range plan.Steps {
		commands = append(commands, s.Command)
	}
	assert.Equal(t, commands, []string{
		"go test ./a -run '^(TestF)$'",
		"pytest tests/test_pkg.py::TestP::test_it",
		"go test ./b -run '^(TestG)$'",
		"go build ./a",
		"go vet ./a",
		"python -m py_compile pkg.py",
	}, "closest tests first, then build and lint; the helper in a test file is no test")
	assert.Equal(t, plan.Steps[0].Reach, 1)
	assert.Equal(t, plan.Steps[2].Reach, 2)
	assert.Equal(t, plan.Untested, []string{"a.U"})
	assert.Equal(t, plan.Notes, []string(nil))

	// A recorded test run adds what it measured: U is covered although no
	// test reaches it in the graph, and TestF failed once on a retry.
	require.NoError(t, mem.SetRuntimeFacts(ctx, "r", runtimefacts.Facts{
		Commit: "abc", Unit: "statements",
		Coverage: map[string]runtimefacts.Coverage{model.NodeID("r", model.KindFunction, "example.com/a.U"): {Covered: 2, Total: 4}},
		Tests:    map[string]runtimefacts.TestRun{model.NodeID("r", model.KindFunction, "example.com/a.TestF"): {Passed: 1, Failed: 1}},
	}))
	plan, err = Build(ctx, mem, "r", rep)
	require.NoError(t, err)
	require.Len(t, plan.Notes, 2)
	assert.Equal(t, plan.Notes[0], "a.U: no test reaches it in the graph, but the run at abc measured 2 of 4 statements covered (50%)")
	assert.Contains(t, plan.Notes[1], "TestF: flaky in the last recorded run (1 passed, 1 failed)")
}

func TestBuildJVM(t *testing.T) {
	ctx := context.Background()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindFile, "build.gradle.kts"), Kind: model.KindFile, Repo: "r", Name: "build.gradle.kts", File: "build.gradle.kts"})
	f := model.NodeID("r", model.KindMethod, "src/main/java/a/Svc.java::Svc.find")
	g.AddNode(model.Node{ID: f, Kind: model.KindMethod, Repo: "r", Name: "find", QualifiedName: "com.acme.Svc.find", File: "src/main/java/a/Svc.java", StartLine: 1, EndLine: 5})
	tst := model.NodeID("r", model.KindMethod, "src/test/java/a/SvcTest.java::SvcTest.findsOne")
	g.AddNode(model.Node{ID: tst, Kind: model.KindMethod, Repo: "r", Name: "findsOne", QualifiedName: "com.acme.SvcTest.findsOne", File: "src/test/java/a/SvcTest.java", StartLine: 1, EndLine: 4, IsTest: true})
	g.AddEdge(model.Edge{From: tst, To: f, Kind: model.EdgeCalls})
	mem := memory.New()
	require.NoError(t, mem.ReplaceRepo(ctx, "r", g))
	rep, err := review.Analyze(ctx, query.Service{R: mem}, "r", []gitx.FileChange{{Path: "src/main/java/a/Svc.java", Ranges: []gitx.LineRange{{Start: 1, End: 5}}}}, review.Options{Depth: 3})
	require.NoError(t, err)
	plan, err := Build(ctx, mem, "r", rep)
	require.NoError(t, err)
	require.Len(t, plan.Steps, 2)
	assert.Equal(t, plan.Steps[0].Command, "./gradlew test --tests 'com.acme.SvcTest.findsOne'")
	assert.Contains(t, plan.Steps[0].Why, "with maven: mvn -q -Dtest='SvcTest#findsOne' test")
	assert.Equal(t, plan.Steps[1].Command, "./gradlew classes testClasses")
}
