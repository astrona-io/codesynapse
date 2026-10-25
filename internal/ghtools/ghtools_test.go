package ghtools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// fakeGH puts a gh on PATH that records its arguments and prints out, or
// fails with stderr when exit is not 0.
func fakeGH(t *testing.T, out string, exit int, stderr string) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "out"), []byte(out), 0o600))
	script := "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\n"
	if exit != 0 {
		script += "echo '" + stderr + "' >&2\nexit " + string(rune('0'+exit)) + "\n"
	} else {
		script += "cat '" + filepath.Join(dir, "out") + "'\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700)) //nolint:gosec // a test double must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

func recorded(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // the test's own file
	require.NoError(t, err)
	return strings.TrimSpace(string(b))
}

const rollup = `{"number":276,"title":"feat(memory): recall token=ghp_abcdefghijklmnopqrstuvwxyz0123456789","state":"OPEN","isDraft":false,
"headRefName":"feat/x","baseRefName":"main","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","reviewDecision":"",
"statusCheckRollup":[
 {"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"},
 {"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"},
 {"__typename":"CheckRun","name":"deploy","status":"COMPLETED","conclusion":"SKIPPED"},
 {"__typename":"CheckRun","name":"Go tests","status":"COMPLETED","conclusion":"FAILURE"},
 {"__typename":"CheckRun","name":"Helm","status":"IN_PROGRESS","conclusion":""},
 {"__typename":"StatusContext","context":"Graphify","state":"SUCCESS"},
 {"__typename":"StatusContext","context":"legacy","state":"PENDING"},
 {"__typename":"StatusContext","context":"lint","state":"ERROR"}
]}`

func TestStatus(t *testing.T) {
	args := fakeGH(t, rollup, 0, "")
	got, err := Status(t.Context(), t.TempDir(), PRStatusIn{Number: 276})
	require.NoError(t, err)
	assert.Equal(t, recorded(t, args), "pr view 276 --json number,title,state,isDraft,headRefName,baseRefName,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup")
	assert.Equal(t, got.Checks.Pass, 3, "check runs and statuses count alike, same names included")
	assert.Equal(t, got.Checks.Fail, 2)
	assert.Equal(t, got.Checks.Pending, 2)
	assert.Equal(t, got.Checks.Skipped, 1)
	assert.False(t, got.Checks.Done)
	assert.Equal(t, strings.Join(got.Checks.Failing, ","), "Go tests,lint")
	assert.Equal(t, strings.Join(got.Checks.Waiting, ","), "Helm,legacy")
	assert.Equal(t, got.MergeState, "BLOCKED")
	assert.False(t, strings.Contains(got.Title, "ghp_abcdefghijklmnopqrstuvwxyz"), "a secret in a title is redacted")

	// No number: the checked-out branch's pull request.
	args = fakeGH(t, rollup, 0, "")
	_, err = Status(t.Context(), t.TempDir(), PRStatusIn{})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(recorded(t, args), "pr view --json"))
}

func TestStatusRefusesBadNumbers(t *testing.T) {
	args := fakeGH(t, rollup, 0, "")
	for _, n := range []int{-1, MaxNumber + 1} {
		_, err := Status(t.Context(), t.TempDir(), PRStatusIn{Number: n})
		assert.True(t, errors.Is(err, ErrInvalid))
	}
	_, statErr := os.Stat(args)
	assert.True(t, os.IsNotExist(statErr), "gh never ran")
}

func TestListArguments(t *testing.T) {
	args := fakeGH(t, `[{"number":1,"title":"a","headRefName":"b","state":"OPEN","isDraft":true}]`, 0, "")
	got, err := List(t.Context(), t.TempDir(), PRListIn{State: "merged", Limit: 500})
	require.NoError(t, err)
	assert.Equal(t, recorded(t, args), "pr list --state merged --limit 30 --json number,title,headRefName,state,isDraft")
	assert.Equal(t, len(got.PullRequests), 1)
	assert.True(t, got.PullRequests[0].Draft)

	for _, in := range []PRListIn{{State: "open; rm -rf /"}, {State: "--web"}, {Limit: -2}} {
		_, err := List(t.Context(), t.TempDir(), in)
		assert.True(t, errors.Is(err, ErrInvalid), "%+v", in)
	}
}

func TestGHFailure(t *testing.T) {
	fakeGH(t, "", 1, "no pull requests found for branch main")
	_, err := Status(t.Context(), t.TempDir(), PRStatusIn{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnavailable))
	assert.True(t, strings.Contains(err.Error(), "no pull requests found"))

	t.Setenv("PATH", t.TempDir())
	_, err = List(t.Context(), t.TempDir(), PRListIn{})
	assert.True(t, errors.Is(err, ErrUnavailable), "gh not installed")
}

func TestRunDecodesStrictly(t *testing.T) {
	fakeGH(t, `[]`, 0, "")
	_, err := Run(t.Context(), t.TempDir(), ToolPRList, json.RawMessage(`{"state":"open","web":true}`))
	assert.True(t, errors.Is(err, ErrInvalid), "unknown fields are refused")
	_, err = Run(t.Context(), t.TempDir(), "gh_pr_merge", nil)
	assert.True(t, errors.Is(err, ErrInvalid), "there is no write tool")
	out, err := Run(t.Context(), t.TempDir(), ToolPRList, nil)
	require.NoError(t, err)
	assert.Equal(t, len(out.(PRListOut).PullRequests), 0)
}

func TestCountChecksCapsNames(t *testing.T) {
	var items []rollupItem
	for i := range 40 {
		items = append(items, rollupItem{Type: "CheckRun", Name: "job-" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Status: "COMPLETED", Conclusion: "FAILURE"})
	}
	c := countChecks(items)
	assert.Equal(t, c.Fail, 40)
	assert.Equal(t, len(c.Failing), MaxNames)
}

// answer is one canned reply of fakeGHRuns: the text it prints when the
// arguments match the shell pattern.
type answer struct{ pattern, text string }

// fakeGHRuns puts a gh on PATH that replies with the first answer whose
// pattern matches its arguments, and fails on any other call.
func fakeGHRuns(t *testing.T, answers ...answer) (argsFile string) {
	t.Helper()
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" >> '" + argsFile + "'\ncase \"$*\" in\n"
	for i, a := range answers {
		f := filepath.Join(dir, "a"+string(rune('0'+i)))
		require.NoError(t, os.WriteFile(f, []byte(a.text), 0o600))
		script += a.pattern + ") cat '" + f + "';;\n"
	}
	script += "*) echo \"unexpected: $*\" >&2; exit 1;;\nesac\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700)) //nolint:gosec // a test double must be executable
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argsFile
}

const failedLog = "Go tests and lint\tRun tests\t2026-10-25T10:00:00.1234567Z === RUN   TestX\n" +
	"Go tests and lint\tRun tests\t2026-10-25T10:00:01.0000000Z --- FAIL: TestX (0.00s)\n" +
	"Go tests and lint\tRun tests\t2026-10-25T10:00:01.0000000Z     x_test.go:12: got 1, want 2\n" +
	"Go tests and lint\tRun tests\t2026-10-25T10:00:01.0000000Z --- FAIL: TestX (0.00s)\n" + // repeated
	"Go tests and lint\tRun tests\t2026-10-25T10:00:02.0000000Z FAIL\tgithub.com/x/y\t0.01s\n" +
	"Go tests and lint\tRun tests\t2026-10-25T10:00:02.0000000Z \x1b[31mpanic:\x1b[0m boom token=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n" +
	"Helm chart\tLint\t2026-10-25T10:00:03.0000000Z linting chart\n" +
	"Helm chart\tLint\t2026-10-25T10:00:04.0000000Z all good here\n" +
	"Helm chart\tLint\t2026-10-25T10:00:05.0000000Z exit status 2\n" +
	"Other job\tstep\t2026-10-25T10:00:05.0000000Z error: not for a failed job\n"

const runJSON = `{"workflowName":"CI","jobs":[
 {"name":"Go tests and lint","conclusion":"failure","steps":[{"name":"Checkout","conclusion":"success"},{"name":"Run tests","conclusion":"failure"}]},
 {"name":"Helm chart","conclusion":"failure","steps":[{"name":"Lint","conclusion":"failure"}]},
 {"name":"Other job","conclusion":"success","steps":[]}]}`

func TestFailureByRun(t *testing.T) {
	args := fakeGHRuns(t, answer{"*--log-failed*", failedLog}, answer{"'run view'*", runJSON})
	got, err := Failure(t.Context(), t.TempDir(), RunFailureIn{Run: 4242})
	require.NoError(t, err)
	assert.Equal(t, got.Run, 4242)
	assert.Equal(t, got.Workflow, "CI")
	require.Equal(t, len(got.Failed), 2, "only failed jobs")
	first := got.Failed[0]
	assert.Equal(t, first.Job, "Go tests and lint")
	assert.Equal(t, strings.Join(first.Steps, ","), "Run tests")
	assert.Equal(t, len(first.Lines), 4, "the error lines, each once, without timestamps")
	assert.Equal(t, first.Lines[0], "--- FAIL: TestX (0.00s)")
	assert.False(t, strings.Contains(strings.Join(first.Lines, "\n"), "ghp_abcdefghijklmnopqrstuvwxyz"), "secrets are redacted")
	assert.False(t, strings.Contains(strings.Join(first.Lines, "\n"), "\x1b"), "colour codes are gone")
	assert.Equal(t, got.Failed[1].Lines, []string{"linting chart", "all good here", "exit status 2"}, "no error-looking line: the last lines")
	for _, l := range append(first.Lines, got.Failed[1].Lines...) {
		assert.False(t, strings.Contains(l, "not for a failed job"))
	}
	assert.True(t, strings.Contains(recorded(t, args), "run view 4242 --log-failed"))
}

func TestFailureLatestRunOfBranch(t *testing.T) {
	args := fakeGHRuns(t,
		answer{"'pr view'*", `{"headRefName":"feat/x"}`},
		answer{"'run list'*", `[{"databaseId":9,"workflowName":"CI","conclusion":"success"},{"databaseId":8,"workflowName":"CI","conclusion":"failure"}]`},
		answer{"*--log-failed*", failedLog}, answer{"'run view'*", runJSON})
	got, err := Failure(t.Context(), t.TempDir(), RunFailureIn{})
	require.NoError(t, err)
	assert.Equal(t, got.Run, 8, "the newest failed run, not the newest run")
	assert.True(t, strings.Contains(recorded(t, args), "run list --branch feat/x --limit 10"))
}

func TestFailureRefusals(t *testing.T) {
	fakeGHRuns(t, answer{"'pr view'*", `{"headRefName":"--web"}`})
	_, err := Failure(t.Context(), t.TempDir(), RunFailureIn{})
	assert.True(t, errors.Is(err, ErrUnavailable), "a branch that reads as an option is not used")
	for _, in := range []RunFailureIn{{Run: -1}, {Number: -1}, {Number: MaxNumber + 1}} {
		_, err := Failure(t.Context(), t.TempDir(), in)
		assert.True(t, errors.Is(err, ErrInvalid), "%+v", in)
	}
}

func TestFailureNoFailedJob(t *testing.T) {
	fakeGHRuns(t, answer{"'run view'*", `{"workflowName":"CI","jobs":[{"name":"a","conclusion":"success"}]}`})
	_, err := Failure(t.Context(), t.TempDir(), RunFailureIn{Run: 1})
	assert.True(t, errors.Is(err, ErrUnavailable))
}
