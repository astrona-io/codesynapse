package gitx

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestValidateRev(t *testing.T) {
	tests := []struct {
		rev string
		ok  bool
	}{
		{"main", true},
		{"origin/main", true},
		{"HEAD~3", true},
		{"v1.2.3", true},
		{"abc123def", true},
		{"", false},
		{"--output=/tmp/x", false},
		{"-c", false},
		{"main..evil", false},
		{"main;rm -rf /", false},
		{"$(id)", false},
	}
	for _, tt := range tests {
		t.Run(tt.rev, func(t *testing.T) {
			err := ValidateRev(tt.rev)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.True(t, errors.Is(err, ErrInvalidRev))
			}
		})
	}
}

const sampleDiff = `diff --git a/pkg/a.go b/pkg/a.go
index 1..2 100644
--- a/pkg/a.go
+++ b/pkg/a.go
@@ -10,2 +10,3 @@ func A() {
@@ -40 +41,0 @@ func B() {
diff --git a/new.go b/new.go
new file mode 100644
--- /dev/null
+++ b/new.go
@@ -0,0 +1,5 @@
diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1,9 +0,0 @@
`

func TestParseDiff(t *testing.T) {
	changes, err := ParseDiff([]byte(sampleDiff))
	require.NoError(t, err)
	require.Len(t, changes, 3)

	assert.Equal(t, changes[0].Path, "pkg/a.go")
	assert.Equal(t, changes[0].Ranges, []LineRange{{10, 12}, {41, 41}})

	assert.Equal(t, changes[1].Path, "new.go")
	assert.True(t, changes[1].Added)
	assert.Equal(t, changes[1].Ranges, []LineRange{{1, 5}})

	assert.Equal(t, changes[2].Path, "gone.go")
	assert.True(t, changes[2].Deleted)
	assert.Len(t, changes[2].Ranges, 0)
}

func TestParseDiffMalformed(t *testing.T) {
	_, err := ParseDiff([]byte("@@ -1 +x @@\n"))
	assert.Error(t, err)
}

func FuzzParseDiff(f *testing.F) {
	f.Add([]byte(sampleDiff))
	f.Fuzz(func(t *testing.T, b []byte) {
		changes, err := ParseDiff(b)
		if err != nil {
			return
		}
		for _, c := range changes {
			for _, r := range c.Ranges {
				if r.End < r.Start {
					t.Fatalf("inverted range %+v", r)
				}
			}
		}
	})
}

func TestParseLog(t *testing.T) {
	out := []byte(recSep + "abc\x1fAda\x1fada@example.com\x1f2026-01-02T03:04:05Z\x1ffix: thing\n\n3\t1\tpkg/a.go\n-\t-\tlogo.png\n" +
		recSep + "def\x1fBob\x1fbob@example.com\x1f2026-01-01T00:00:00Z\x1finit\n")
	commits, err := ParseLog(out)
	require.NoError(t, err)
	require.Len(t, commits, 2)
	assert.Equal(t, commits[0].Subject, "fix: thing")
	assert.Equal(t, commits[0].Files, []FileStat{{"pkg/a.go", 3, 1}, {"logo.png", 0, 0}})
	assert.Len(t, commits[1].Files, 0)
}

type fakeResolver map[string][]net.IPAddr

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestValidateRemote(t *testing.T) {
	res := fakeResolver{
		"github.com":     {{IP: net.ParseIP("140.82.121.3")}},
		"internal.corp":  {{IP: net.ParseIP("10.0.0.5")}},
		"metadata.local": {{IP: net.ParseIP("169.254.169.254")}},
	}
	tests := []struct {
		url     string
		private bool
		ok      bool
	}{
		{"https://github.com/acme/app.git", false, true},
		{"https://github.com/acme/app/", false, true},
		{"http://github.com/acme/app", false, false},
		{"git@github.com:acme/app.git", false, false},
		{"ssh://github.com/acme/app", false, false},
		{"file:///etc", false, false},
		{"https://user:pass@github.com/acme/app", false, false},
		{"https://internal.corp/acme/app", false, false},
		{"https://internal.corp/acme/app", true, true},
		{"https://metadata.local/latest", false, false},
		{"https://github.com/acme/app?x=1", false, false},
		{"https://github.com/--upload-pack=x", false, true}, // path, not an option: passed after --
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			_, err := ValidateRemote(context.Background(), tt.url, RemoteOptions{AllowPrivate: tt.private, Resolver: res})
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.True(t, errors.Is(err, ErrInvalidRemote), "got %v", err)
			}
		})
	}
}

func TestRemoteEnvScopesToken(t *testing.T) {
	env := strings.Join(remoteEnv("https://github.com/acme/app", Auth{Host: "github.com", Token: "tok"}), "\n")
	assert.Contains(t, env, "http.https://github.com/.extraHeader")
	assert.NotContains(t, env, "tok\n", "token is base64-encoded in a header")
	other := strings.Join(remoteEnv("https://gitlab.com/acme/app", Auth{Host: "github.com", Token: "tok"}), "\n")
	assert.NotContains(t, other, "extraHeader", "token only sent to its host")
	assert.Contains(t, other, "protocol.allow")
}

func TestSanitizeURL(t *testing.T) {
	assert.Equal(t, SanitizeURL("https://x:y@github.com/a/b"), "https://github.com/a/b")
}

func TestWorktreeCommit(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", "init"}} {
		_, err := Run(ctx, dir, args...)
		require.NoError(t, err)
	}
	head := HeadCommit(ctx, dir)
	wt := filepath.Join(t.TempDir(), "wt")
	require.NoError(t, AddWorktree(ctx, dir, wt, head))
	require.NoError(t, os.WriteFile(filepath.Join(wt, "a.go"), []byte("package a\n"), 0o600))
	id, err := CommitFiles(ctx, wt, []string{"a.go"}, "fix: add a", Author{Name: "CodeSynapse", Email: "codesynapse@localhost"})
	require.NoError(t, err)
	assert.True(t, id != "" && id != head)
	_, err = CommitFiles(ctx, wt, []string{"../x"}, "m", Author{})
	assert.Error(t, err)
	require.NoError(t, RemoveWorktree(ctx, dir, wt))
	assert.Equal(t, HeadCommit(ctx, dir), head) // the indexed checkout is untouched
}

func TestPushBranchRefusesUnsafeTargets(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ remote, branch string }{
		{"https://github.com/o/r", "main"},
		{"https://github.com/o/r", "codesynapse/../main"},
		{"file:///tmp/r", "codesynapse/fix-1"},
	} {
		assert.Error(t, PushBranch(ctx, t.TempDir(), tc.remote, tc.branch, Auth{}))
	}
}

func TestHTTPSRemote(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:owner/repo.git":         "https://github.com/owner/repo.git",
		"ssh://git@github.com/owner/repo":       "https://github.com/owner/repo",
		"ssh://git@gitlab.example:2222/g/r.git": "https://gitlab.example/g/r.git",
		"https://github.com/owner/repo.git":     "https://github.com/owner/repo.git",
		"":                                      "",
	} {
		assert.Equal(t, HTTPSRemote(in), want, in)
	}
}
