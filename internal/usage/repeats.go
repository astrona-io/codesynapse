package usage

import "strings"

// Repeats and re-sends (docs/PLATFORM_STATS_PLAN.md, token savings without
// the prompt cache). The plugin's hooks report, per tool, the answered line
// ranges that overlapped a range the session had already been answered or
// read (RepeatTool), and the answer tokens of the other answers times the
// turns the session ran after them (ResendTool). Calls holds the count. A
// report that carries the repeat counts at all also adds one to RepeatAware,
// so the server can tell "no repeats" from a hook too old to count them.

// RepeatAware is the counter of reports that carried repeat counts.
const RepeatAware = "(ranges:repeat-aware)"

const (
	repeatPrefix = "(repeat:"
	resendPrefix = "(resend:"
)

// RepeatTool is the pseudo tool repeat ranges of tool are counted under.
func RepeatTool(tool string) string { return repeatPrefix + tool + ")" }

// ResendTool is the pseudo tool re-sent answer tokens of tool are counted under.
func ResendTool(tool string) string { return resendPrefix + tool + ")" }

// RepeatToolOf returns the tool of a RepeatTool pseudo tool.
func RepeatToolOf(tool string) (string, bool) { return cutPseudo(tool, repeatPrefix) }

// ResendToolOf returns the tool of a ResendTool pseudo tool.
func ResendToolOf(tool string) (string, bool) { return cutPseudo(tool, resendPrefix) }

func cutPseudo(tool, prefix string) (string, bool) {
	t, ok := strings.CutPrefix(tool, prefix)
	t, ok2 := strings.CutSuffix(t, ")")
	return t, ok && ok2 && t != ""
}

// isRepeatCounter reports whether tool is one of the counters above.
func isRepeatCounter(tool string) bool {
	if tool == RepeatAware {
		return true
	}
	_, rep := RepeatToolOf(tool)
	_, res := ResendToolOf(tool)
	return rep || res
}
