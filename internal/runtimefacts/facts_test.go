package runtimefacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const goProfile = `mode: atomic
example.com/app/internal/svc/svc.go:3.20,5.2 2 4
example.com/app/internal/svc/svc.go:5.2,7.3 1 0
example.com/app/internal/svc/svc.go:10.20,12.2 3 0
example.com/app/internal/svc/svc.go:10.20,12.2 3 1
example.com/other/mod.go:1.1,2.2 1 1
`

const lcov = `TN:
SF:/home/ci/work/web/src/util.ts
FN:1,add
DA:1,1
DA:2,1
DA:3,0
end_of_record
`

const junit = `<?xml version="1.0"?>
<testsuites>
  <testsuite name="example.com/app/internal/svc">
    <testcase classname="example.com/app/internal/svc" name="TestDo" time="0.010"/>
    <testcase classname="example.com/app/internal/svc" name="TestDo/edge_case" time="0.002"><failure message="boom"/></testcase>
    <testcase classname="example.com/app/internal/svc" name="TestDo" time="0.011"/>
    <testcase classname="example.com/app/internal/svc" name="TestSkip"><skipped/></testcase>
    <testcase classname="example.com/app/internal/svc" name="TestGone"/>
  </testsuite>
  <testsuite name="pytest">
    <testcase classname="tests.test_api.TestRoutes" name="test_get[1]" time="0.5"/>
  </testsuite>
</testsuites>`

func graph() Source {
	return NodeSource([]model.Node{
		{ID: "r|File|internal/svc/svc.go", Kind: model.KindFile, File: "internal/svc/svc.go"},
		{ID: "r|File|internal/svc/svc_test.go", Kind: model.KindFile, File: "internal/svc/svc_test.go"},
		{ID: "r|File|web/src/util.ts", Kind: model.KindFile, File: "web/src/util.ts"},
		{ID: "r|File|tests/test_api.py", Kind: model.KindFile, File: "tests/test_api.py"},
		{ID: "r|Function|svc.Do", Kind: model.KindFunction, Name: "Do", File: "internal/svc/svc.go", StartLine: 3, EndLine: 7},
		{ID: "r|Function|svc.Never", Kind: model.KindFunction, Name: "Never", File: "internal/svc/svc.go", StartLine: 10, EndLine: 12},
		{ID: "r|Function|svc.TestDo", Kind: model.KindFunction, Name: "TestDo", File: "internal/svc/svc_test.go", StartLine: 1, EndLine: 9, IsTest: true},
		{ID: "r|Function|svc.TestSkip", Kind: model.KindFunction, Name: "TestSkip", File: "internal/svc/svc_test.go", StartLine: 10, EndLine: 12, IsTest: true},
		{ID: "r|Function|util.add", Kind: model.KindFunction, Name: "add", File: "web/src/util.ts", StartLine: 1, EndLine: 3},
		{ID: "r|Method|tests.test_api.TestRoutes.test_get", Kind: model.KindMethod, Name: "test_get", File: "tests/test_api.py", StartLine: 4, EndLine: 6, IsTest: true},
	})
}

func TestMap(t *testing.T) {
	ctx := context.Background()
	prof, err := ParseCoverage(strings.NewReader(goProfile))
	require.NoError(t, err)
	assert.Equal(t, prof.Unit, "statements")
	assert.Len(t, prof.Blocks, 4, "a repeated block is merged")
	lc, err := ParseCoverage(strings.NewReader(lcov))
	require.NoError(t, err)
	assert.Equal(t, lc.Unit, "lines")
	cases, err := ParseJUnit(strings.NewReader(junit))
	require.NoError(t, err)
	require.Len(t, cases, 6)

	f, err := Map(ctx, graph(), Input{Profiles: []Profile{prof, lc}, Tests: cases})
	require.NoError(t, err)
	assert.Equal(t, f.Coverage["r|Function|svc.Do"], Coverage{Covered: 2, Total: 3})
	assert.Equal(t, f.Coverage["r|Function|svc.Never"], Coverage{Covered: 3, Total: 3}, "a hit in a merged profile wins")
	assert.Equal(t, f.Coverage["r|Function|util.add"], Coverage{Covered: 2, Total: 3}, "an absolute LCOV path maps by suffix")
	assert.Equal(t, f.UnmatchedFiles, 1, "another module's file")

	do := f.Tests["r|Function|svc.TestDo"]
	assert.Equal(t, do.Passed, 2)
	assert.Equal(t, do.Failed, 1, "a subtest counts for its test function")
	assert.True(t, do.Flaky())
	assert.Equal(t, do.Status(), "flaky")
	assert.Equal(t, f.Tests["r|Function|svc.TestSkip"].Status(), "skipped")
	assert.Equal(t, f.Tests["r|Method|tests.test_api.TestRoutes.test_get"].Status(), "passed", "pytest class and parameters")
	assert.Equal(t, f.UnmatchedTests, 1, "TestGone has no node")
	assert.Equal(t, Coverage{Covered: 2, Total: 3}.Percent(), 66)
}

func TestParseErrors(t *testing.T) {
	tests := []struct{ name, in string }{
		{"go range", "mode: set\nx.go:3.1 1 1\n"},
		{"go fields", "mode: set\nx.go:3.1,4.1 1\n"},
		{"go inverted", "mode: set\nx.go:9.1,4.1 1 1\n"},
		{"lcov outside file", "DA:1,1\n"},
		{"not a report", "hello\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCoverage(strings.NewReader(tt.in))
			assert.Error(t, err)
		})
	}
	_, err := ParseJUnit(strings.NewReader("<testsuite><testcase"))
	assert.Error(t, err)
}

func TestLoadAndDiscover(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "cover.out"), []byte(goProfile), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "test-results"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "test-results", "junit.xml"), []byte(junit), 0o600))
	files := Discover(root)
	assert.Len(t, files.Coverage, 1)
	assert.Len(t, files.JUnit, 1)

	f, err := Load(context.Background(), graph(), files)
	require.NoError(t, err)
	assert.Equal(t, f.Origin, "local")
	assert.Equal(t, f.Sources, []string{"coverage cover.out", "junit junit.xml"})
	assert.False(t, f.MeasuredAt.IsZero())
	assert.Len(t, f.Coverage, 2)

	require.NoError(t, os.WriteFile(filepath.Join(root, "cover.out"), []byte("mode: set\nbad"), 0o600))
	_, err = Load(context.Background(), graph(), files)
	assert.Error(t, err)
	assert.True(t, Discover(t.TempDir()).Empty())
}

func TestLimits(t *testing.T) {
	var b strings.Builder
	b.WriteString("<testsuite>")
	for range MaxTestCases + 1 {
		b.WriteString(`<testcase name="T"/>`)
	}
	b.WriteString("</testsuite>")
	_, err := ParseJUnit(strings.NewReader(b.String()))
	assert.True(t, errors.Is(err, ErrTooLarge))
}

func FuzzParseCoverage(f *testing.F) {
	f.Add([]byte(goProfile))
	f.Add([]byte(lcov))
	f.Add([]byte("mode: set\nC:\\work\\x.go:1.1,1.9 1 1\n"))
	f.Add([]byte("SF:../../etc/passwd\nDA:1,1\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParseCoverage(strings.NewReader(string(b)))
		if err != nil {
			return
		}
		for _, bl := range p.Blocks {
			if bl.StartLine < 1 || bl.EndLine < bl.StartLine || bl.Units < 0 {
				t.Fatalf("bad block %+v", bl)
			}
			if strings.HasPrefix(bl.File, "/") || strings.Contains("/"+bl.File+"/", "/../") {
				t.Fatalf("unclean path %q", bl.File)
			}
		}
	})
}

func FuzzParseJUnit(f *testing.F) {
	f.Add([]byte(junit))
	f.Add([]byte(`<!DOCTYPE x [<!ENTITY e SYSTEM "file:///etc/passwd">]><testcase name="&e;"/>`))
	f.Fuzz(func(t *testing.T, b []byte) {
		cases, err := ParseJUnit(strings.NewReader(string(b)))
		if err != nil {
			return
		}
		for _, c := range cases {
			if c.Name == "" || len(c.Name) > maxNameLen || c.Millis < 0 {
				t.Fatalf("bad case %+v", c)
			}
		}
	})
}

func TestNotes(t *testing.T) {
	f := Facts{Commit: "0123456789abcdef", Coverage: map[string]Coverage{"a": {Covered: 0, Total: 5}}, Tests: map[string]TestRun{"p": {Passed: 3}, "x": {Failed: 2}}}
	assert.Equal(t, f.At(), "0123456789ab")
	assert.False(t, f.StaleFor("0123456"), "a short revision of the same commit")
	assert.False(t, f.StaleFor(""), "unknown revision")
	assert.True(t, f.StaleFor("fedcba98"))
	note, ok := f.CoverageNote("a")
	assert.True(t, ok)
	assert.Equal(t, note, "0 of 5 statements covered (0%)")
	_, ok = f.RunNote("p")
	assert.False(t, ok, "a passing test needs no note")
	note, ok = f.RunNote("x")
	assert.True(t, ok)
	assert.Equal(t, note, "failed in the last recorded run")
	assert.Equal(t, Facts{}.At(), "unknown")
}
