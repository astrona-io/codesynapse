package codeowners

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The examples of GitHub's CODEOWNERS documentation.
const sample = `# Default owners for everything
*       @global-owner1 @global-owner2

*.js    @js-owner #This is an inline comment.
*.go docs@example.com
*.txt @octo-org/octocats
/build/logs/ @doctocat
docs/* docs@example.com @docs-team
apps/ @octocat
/docs/ @doctocat
/scripts/ @doctocat @octocat
**/logs @octocat
/apps/ @octocat
/apps/github
[Section]
/web/** @web-team
`

func TestOwners(t *testing.T) {
	s := Compile(Parse([]byte(sample)))
	for path, want := range map[string]string{
		"README.md":                         "@global-owner1 @global-owner2",
		"web/app.js":                        "@web-team",
		"src/app.js":                        "@js-owner",
		"main.go":                           "", // owned by an e-mail address only, which is dropped
		"notes/todo.txt":                    "@octo-org/octocats",
		"build/logs/today.log":              "@octocat", // **/logs comes later than /build/logs/
		"deep/build/logs/x":                 "@octocat",
		"tmp/build/logs.txt":                "@octo-org/octocats",
		"docs/getting-started.md":           "@doctocat",
		"scripts/run.sh":                    "@doctocat @octocat",
		"api/logs/today.log":                "@octocat",
		"apps/web/main.c":                   "@octocat",
		"apps/github/x.c":                   "", // the last match has no owners
		"src/docs/build-app/troubleshoot.c": "@global-owner1 @global-owner2",
	} {
		assert.Equal(t, strings.Join(s.Owners(path), " "), want, path)
	}
}

func TestDocsStarIsNotRecursive(t *testing.T) {
	s := Compile(Parse([]byte("docs/* @docs\n")))
	assert.Equal(t, s.Owners("docs/getting-started.md"), []string{"@docs"})
	assert.Len(t, s.Owners("docs/build-app/troubleshooting.md"), 0)
}

func TestParseBounds(t *testing.T) {
	var b strings.Builder
	for range maxRules + 10 {
		b.WriteString("x/ @a\n")
	}
	b.WriteString("!negated @a\n")
	assert.Len(t, Parse([]byte(b.String())), maxRules)
	rules := Parse([]byte("a @user @org/team @bad..handle someone@example.com\n"))
	require.Len(t, rules, 1)
	assert.Equal(t, rules[0].Owners, []string{"@user", "@org/team"})
	assert.True(t, Compile(nil).Empty())
	assert.Len(t, Compile(nil).Owners("x"), 0)
}

func TestRead(t *testing.T) {
	root := t.TempDir()
	rules, at := Read(root)
	assert.Len(t, rules, 0)
	assert.Equal(t, at, "")
	require.NoError(t, os.WriteFile(filepath.Join(root, "CODEOWNERS"), []byte("* @root\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".github"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".github", "CODEOWNERS"), []byte("* @github\n"), 0o600))
	rules, at = Read(root)
	assert.Equal(t, at, ".github/CODEOWNERS", "GitHub's order")
	assert.Equal(t, rules, []model.OwnerRule{{Pattern: "*", Owners: []string{"@github"}}})
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(sample))
	f.Add([]byte("**/a/**/b/*.?? @x\n[s][2] @y\n"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		s := Compile(Parse(data))
		_ = s.Owners("a/b/c.go")
	})
}
