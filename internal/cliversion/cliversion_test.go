package cliversion

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Release
		ok   bool
	}{
		{"v0.0.23", Release{0, 0, 23}, true},
		{"1.2.3", Release{1, 2, 3}, true},
		{" v10.20.30 ", Release{10, 20, 30}, true},
		{"dev", Release{}, false},
		{"", Release{}, false},
		{"4c1dcd7", Release{}, false},
		{"v0.0.23-5-g4c1dcd7", Release{}, false}, // git describe past a tag
		{"v0.0.23-dirty", Release{}, false},
		{"v0.1.0-rc.1", Release{}, false},
		{"v0.0.23+meta", Release{}, false},
		{"v1.2", Release{}, false},
		{"v1.2.3.4", Release{}, false},
		{"v01.2.3", Release{}, false},
		{"v-1.2.3", Release{}, false},
		{"v1..3", Release{}, false},
		{"v9999999.0.0", Release{}, false},
		{"vv1.2.3", Release{}, false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := Parse(tc.in)
			assert.Equal(t, ok, tc.ok)
			assert.Equal(t, got, tc.want)
		})
	}
}

func TestBehind(t *testing.T) {
	for _, tc := range []struct {
		cli, newest string
		want        bool
	}{
		{"v0.0.23", "v0.0.24", true},
		{"v0.0.23", "v0.1.0", true},
		{"v0.9.9", "v1.0.0", true},
		{"v0.0.24", "v0.0.24", false},
		{"v0.0.25", "v0.0.24", false}, // a newer CLI than the server is fine
		{"v0.0.10", "v0.0.9", false},  // numeric, not string order
		{"dev", "v0.0.24", false},     // a dev build never nags
		{"v0.0.23-3-gabc1234", "v0.0.24", false},
		{"v0.0.23-dirty", "v0.0.24", false},
		{"v0.0.23", "dev", false}, // nor does a dev server
		{"v0.0.23", "", false},
		{"v0.0.23", "v0.0.24-2-gabc1234", false},
	} {
		t.Run(tc.cli+" vs "+tc.newest, func(t *testing.T) {
			assert.Equal(t, Behind(tc.cli, tc.newest), tc.want)
		})
	}
}

func TestNotices(t *testing.T) {
	n := UpgradeNotice("v0.0.23", "0.0.24")
	assert.Equal(t, n, "You are on an old version of CodeSynapse (v0.0.23; the newest is v0.0.24). Please upgrade to the newest version of CodeSynapse, then run `codesynapse doctor --fix` and start a new session to pick up the missing changes from this update.")
	assert.Contains(t, ForAgent(n), `tell the user this in these words: "You are on an old version`)
	assert.Contains(t, PluginBehindNotice("v0.0.24", 2), "2 hook(s) from this update are missing. Please run `codesynapse doctor --fix`")
	assert.Contains(t, PluginAheadNotice("v0.0.23"), "Please upgrade to the newest version of CodeSynapse")
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"v0.0.23", "1.2.3", "dev", "v0.0.23-5-gabc", "v01.2.3", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, ok := Parse(s)
		if !ok {
			return
		}
		// A parsed release prints as a tag that parses back to itself.
		back, ok := Parse(r.String())
		if !ok || back != r {
			t.Fatalf("%q parsed as %v, which does not round-trip", s, r)
		}
		if r.Less(r) || strings.ContainsAny(r.String(), "-+ ") {
			t.Fatalf("%q: bad release %v", s, r)
		}
	})
}
