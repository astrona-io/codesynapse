package taskeval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cases.jsonl")
	none, err := ReadCases(path)
	require.NoError(t, err)
	assert.Len(t, none, 0, "no file, no cases")

	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	require.NoError(t, SaveCase(path, Case{Session: "a", Task: "first try", Base: "abc", Truth: []string{"x.go"}, At: at}))
	require.NoError(t, SaveCase(path, Case{Session: "b", Task: "other", Base: "abc", Truth: []string{"y.go"}, At: at.Add(time.Minute)}))
	require.NoError(t, SaveCase(path, Case{Session: "a", Task: "first try", Base: "abc", Truth: []string{"x.go", "z.go"}, Added: 1, At: at.Add(2 * time.Minute)}))
	got, err := ReadCases(path)
	require.NoError(t, err)
	require.Len(t, got, 2, "a session's later stop replaces its case")
	assert.Equal(t, got[1].Truth, []string{"x.go", "z.go"})
	assert.Equal(t, got[1].AsTask(), Task{Commit: "abc", Subject: "first try", Truth: []string{"x.go", "z.go"}, Added: 1})
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, st.Mode().Perm(), os.FileMode(0o600), "the cases stay private to the user")

	for i := range MaxCases + 3 {
		require.NoError(t, SaveCase(path, Case{Session: string(rune('A'+i%26)) + string(rune('a'+i/26)), Task: "t", Base: "abc", Truth: []string{"x.go"}, At: at.Add(time.Duration(i+10) * time.Minute)}))
	}
	got, err = ReadCases(path)
	require.NoError(t, err)
	assert.Equal(t, len(got), MaxCases, "the newest are kept")

	require.NoError(t, os.WriteFile(path, []byte(`{"session":"x","task":"","base":"abc","truth":["a.go"]}`+"\n\n"), 0o600))
	got, err = ReadCases(path)
	require.NoError(t, err)
	assert.Len(t, got, 0, "a case without a task is not one")
	require.NoError(t, os.WriteFile(path, []byte("not json\n"), 0o600))
	_, err = ReadCases(path)
	assert.True(t, err != nil)
}

func TestMerge(t *testing.T) {
	a := Report{Results: []Result{{Task: Task{Subject: "a"}, Scores: map[string]Score{ToolMinimalContext: {Recall10: 1, Tokens: 100}, ToolSearch: {}}}}}
	b := Report{Results: []Result{{Task: Task{Subject: "b"}, Scores: map[string]Score{ToolMinimalContext: {Recall10: 0, Tokens: 300}, ToolSearch: {Recall10: 0.5}}}}}
	m := Merge("r", []Report{a, b})
	assert.Equal(t, m.Tasks, 2)
	assert.Equal(t, m.Summary[ToolMinimalContext], Summary{Recall10: 0.5, Hit10: 0.5, Tokens: 200})
	assert.Equal(t, m.Summary[ToolSearch].Recall10, 0.25)
}
