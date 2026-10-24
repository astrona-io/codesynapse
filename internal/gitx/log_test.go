package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestUnquotePath(t *testing.T) {
	for in, want := range map[string]string{
		`plain.go`:                      `plain.go`,
		`dir/sub file.go`:               `dir/sub file.go`,
		`"caf\303\251.go"`:              "café.go",
		`"tab\there.go"`:                "tab\there.go",
		`"new\nline.go"`:                "new\nline.go",
		`"quo\"te.go"`:                  `quo"te.go`,
		`"back\\slash.go"`:              `back\slash.go`,
		`"\346\227\245\346\234\254.go"`: "日本.go",
		`"unterminated\`:                `"unterminated\`,
		`"bad\q.go"`:                    `"bad\q.go"`,
		`""`:                            ``,
	} {
		assert.Equal(t, unquotePath(in), want, in)
	}
}

// Real git output for awkward file names: the paths ParseLog returns are the
// ones on disk, so history can be matched to graph files.
func TestLogReturnsTheRealPathOfUnicodeAndEscapedNames(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		_, err := Run(context.Background(), dir, args...)
		require.NoError(t, err)
	}
	run("init", "-q")
	run("config", "user.email", "a@example.com")
	run("config", "user.name", "A")
	run("config", "commit.gpgsign", "false")
	names := []string{"café.go", "日本語.go", "tab\there.go", "quo\"te.go", "back\\slash.go", "sub dir/plain.go"}
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte("package x\n"), 0o600))
	}
	run("add", "-A")
	run("commit", "-q", "-m", "add files")

	commits, err := Log(context.Background(), dir, 5)
	require.NoError(t, err)
	require.Len(t, commits, 1)
	var got []string
	for _, f := range commits[0].Files {
		got = append(got, f.Path)
		assert.Equal(t, f.Added, 1, f.Path)
	}
	slices.Sort(got)
	slices.Sort(names)
	assert.Equal(t, strings.Join(got, "|"), strings.Join(names, "|"))
}
