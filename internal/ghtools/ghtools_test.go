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
