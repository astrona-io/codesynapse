// Package cliversion compares the codesynapse CLI's version with the newest
// one a server announces, and words the notice an outdated CLI gives the
// person and their agent. CLI and server are released together from one tag,
// so a server's version is the newest CLI the person should run.
//
// Only release tags (v1.2.3) are compared. A development build ("dev", a
// commit, `git describe` past a tag, a dirty tree, a pre-release) is never
// behind and never nags.
package cliversion

import (
	"fmt"
	"strconv"
	"strings"
)

// maxLen bounds what Parse looks at; tags are short.
const maxLen = 32

// Release is a parsed release tag.
type Release struct{ Major, Minor, Patch int }

// Parse reads a release tag: v1.2.3 or 1.2.3, nothing before or after it.
func Parse(s string) (Release, bool) {
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return Release{}, false
	}
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Release{}, false
	}
	var n [3]int
	for i, p := range parts {
		if p == "" || len(p) > 6 || len(p) > 1 && p[0] == '0' {
			return Release{}, false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return Release{}, false
			}
		}
		n[i], _ = strconv.Atoi(p) //nolint:errcheck // at most six digits, checked above
	}
	return Release{n[0], n[1], n[2]}, true
}

func (r Release) String() string { return fmt.Sprintf("v%d.%d.%d", r.Major, r.Minor, r.Patch) }

// Less reports whether r is an older release than o.
func (r Release) Less(o Release) bool {
	if r.Major != o.Major {
		return r.Major < o.Major
	}
	if r.Minor != o.Minor {
		return r.Minor < o.Minor
	}
	return r.Patch < o.Patch
}

// Behind reports whether cli is an older release than newest. It is false
// whenever either one is not a release.
func Behind(cli, newest string) bool {
	c, ok := Parse(cli)
	if !ok {
		return false
	}
	n, ok := Parse(newest)
	return ok && c.Less(n)
}

// IsRelease reports whether v is a release tag rather than a development build.
func IsRelease(v string) bool {
	_, ok := Parse(v)
	return ok
}

// FixCommand repairs the local setup after an upgrade.
const FixCommand = "codesynapse doctor --fix"

// UpgradeNotice is what the person is told when their CLI is behind the
// server: upgrade, then repair the setup, then start a new session.
func UpgradeNotice(cli, newest string) string {
	c, _ := Parse(cli)    //nolint:errcheck // callers checked Behind
	n, _ := Parse(newest) //nolint:errcheck // same
	return fmt.Sprintf("You are on an old version of CodeSynapse (%s; the newest is %s). Please upgrade to the newest version of CodeSynapse, then run `%s` and start a new session to pick up the missing changes from this update.", c, n, FixCommand)
}

// PluginBehindNotice is what the person is told when the Claude Code plugin
// lacks hooks this CLI ships.
func PluginBehindNotice(cli string, missing int) string {
	return fmt.Sprintf("The CodeSynapse plugin for Claude Code is older than your codesynapse CLI (%s): %d hook(s) from this update are missing. Please run `%s` to fix the missing changes from this update, then start a new session.", cli, missing, FixCommand)
}

// PluginAheadNotice is what the person is told when the plugin runs hooks
// this CLI does not know: the CLI is the old part.
func PluginAheadNotice(cli string) string {
	return fmt.Sprintf("You are on an old version of CodeSynapse: the Claude Code plugin runs hooks your codesynapse CLI (%s) does not have. Please upgrade to the newest version of CodeSynapse, then run `%s` and start a new session.", cli, FixCommand)
}

// ForAgent wraps a notice for the agent's context: the agent passes it on
// to the person in these words.
func ForAgent(notice string) string {
	return "CodeSynapse update notice: before your first answer in this session, tell the user this in these words: \"" + notice + "\""
}
