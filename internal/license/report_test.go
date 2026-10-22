package license

import (
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestBuild(t *testing.T) {
	rep := Build(Found{"Apache-2.0", "LICENSE"}, []Dependency{
		{Ecosystem: Go, Name: "stdlib", Version: "v1.27.3", License: "BSD-3-Clause"},
		{Ecosystem: Go, Name: "github.com/spf13/cobra", Version: "v1.10.2", License: "Apache-2.0", Source: "deps.dev"},
		{Ecosystem: NPM, Name: "vitest", Version: "1.0.0", License: "MIT", Dev: true},
		{Ecosystem: NPM, Name: "gpl-dev", Version: "1.0.0", License: "GPL-3.0", Dev: true},
		{Ecosystem: NPM, Name: "gpl-runtime", Version: "2.0.0", License: "GPL-3.0", Indirect: true},
		{Ecosystem: Go, Name: "example.com/agpl", Version: "v1.0.0", License: "AGPL-3.0"},
		{Ecosystem: PyPI, Name: "mystery", Version: "0.1"},
	}, false)
	assert.Equal(t, rep.Category, Permissive)
	assert.Equal(t, rep.Total, 6, "stdlib is not counted")
	assert.Equal(t, rep.OK, 2)
	assert.Equal(t, rep.Conflict, 2)
	assert.Equal(t, rep.Review, 2)
	assert.Equal(t, rep.Unknown, 1)
	require.Len(t, rep.Issues, 4)
	assert.Equal(t, rep.Issues[0].Name, "example.com/agpl", "direct shipped conflicts first")
	assert.Equal(t, rep.Issues[1].Name, "gpl-runtime")
	assert.Equal(t, rep.Issues[1].Verdict.Level, Conflict)
	assert.Equal(t, rep.Issues[2].Verdict.Level, Review)
	assert.Equal(t, rep.ByLicense[0], Count{"GPL-3.0", StrongCopyleft, 2})

	md := Markdown("demo", rep)
	for _, want := range []string{"**Project licence:** Apache-2.0 (permissive), from LICENSE", "| conflict | Go example.com/agpl@v1.0.0 (direct) | AGPL-3.0 |", "enable the deps.dev lookup"} {
		assert.True(t, strings.Contains(md, want), want)
	}
}

func TestBuildWithoutProjectLicence(t *testing.T) {
	rep := Build(Found{}, []Dependency{{Ecosystem: NPM, Name: "lodash", Version: "4.17.21", License: "MIT"}}, true)
	assert.Equal(t, rep.Category, Unknown)
	assert.Equal(t, rep.OK, 1)
	assert.True(t, strings.Contains(Markdown("x", rep), "none found"))
}
