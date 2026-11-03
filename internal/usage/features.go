package usage

import (
	"slices"
	"strings"
)

// Features that are not MCP tools are timed under these pseudo tools
// (docs/plans/FEATURE_HEALTH_PLAN.md): the model-backed work the server or
// the hooks run, and the hooks themselves. Calls is the number of timed runs.
// MCP tools, including the ones the bridge answers on the developer's
// machine (ask_local, local_draft, read_ranges, the git tools), keep their
// own tool names.
const (
	FeaturePreBrief      = "(feature:pre-brief)"
	FeatureSourceExtract = "(feature:source-extract)"
	FeatureGatewayLocal  = "(feature:gateway-local)"
	FeatureShorten       = "(feature:prompt-shortening)"
	HookSessionStart     = "(hook:session-start)"
	HookPromptRecall     = "(hook:prompt-recall)"
	HookStop             = "(hook:stop)"
	HookRetrievalReport  = "(hook:retrieval-report)"
)

// ModelFeatures are the pseudo features a self-hosted model answers.
var ModelFeatures = []string{FeaturePreBrief, FeatureSourceExtract, FeatureGatewayLocal, FeatureShorten}

// HookFeatures are the hooks' own timings.
var HookFeatures = []string{HookSessionStart, HookPromptRecall, HookStop, HookRetrievalReport}

// ReportedFeatures are the pseudo features the plugin's hooks may report
// timings of (the retrieval report); the server times the others itself.
var ReportedFeatures = append([]string{FeaturePreBrief}, HookFeatures...)

// FeatureName is the short name reports use for a pseudo feature
// ("pre-brief" for FeaturePreBrief).
func FeatureName(tool string) string {
	for _, p := range []string{"(feature:", "(hook:"} {
		if n, ok := strings.CutPrefix(tool, p); ok {
			return strings.TrimSuffix(n, ")")
		}
	}
	return tool
}

// ReportedFeature returns the pseudo feature a hook report names by its
// short name, and whether it is one the hooks may report.
func ReportedFeature(name string) (string, bool) {
	i := slices.IndexFunc(ReportedFeatures, func(f string) bool { return FeatureName(f) == name })
	if i < 0 {
		return "", false
	}
	return ReportedFeatures[i], true
}

// IsFeature reports whether tool is one of the pseudo features.
func IsFeature(tool string) bool {
	return slices.Contains(ModelFeatures, tool) || slices.Contains(HookFeatures, tool)
}
