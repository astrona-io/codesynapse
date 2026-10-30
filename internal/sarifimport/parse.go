// Package sarifimport reads SARIF 2.1.0 logs of external security linters
// (gitleaks, Semgrep, zizmor, kube-linter, …) so their findings can join
// CodeSynapse's security report. A log comes from CI and is untrusted: the
// reader is bounded, keeps only what it maps (tool, rule, severity,
// repository-relative location, a short redacted message, the rule's
// https help link) and never reads snippets or other source text.
package sarifimport

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/redact"
)

// Limits on one log and what is kept of it.
const (
	MaxLogBytes = 32 << 20
	// MaxRuns bounds the runs of one log and the tools kept per namespace.
	MaxRuns = 20
	// MaxFindings bounds the findings of one log.
	MaxFindings = 20_000
	maxRules    = 20_000
	maxArgs     = 10
	// MaxMessage is the length, in runes, a message is cut to.
	MaxMessage   = 300
	maxNameRunes = 100
	maxRuleRunes = 200
	maxPathLen   = 4096
	maxHelpURI   = 512
	maxLine      = 10_000_000
)

var (
	// ErrTooLarge reports a log over the limits above.
	ErrTooLarge = errors.New("SARIF log exceeds the size limits")
	// ErrInvalid reports a log that is not SARIF 2.1.0.
	ErrInvalid = errors.New("not a SARIF 2.1.0 log")
)

// Finding is one result of an external tool, as it is kept.
type Finding struct {
	// RuleID is the tool's own rule identifier.
	RuleID string `json:"rule_id"`
	// Severity is info, low, medium, high or critical (model.Severities).
	Severity string `json:"severity"`
	// Level is the SARIF level the severity came from: error, warning,
	// note or none.
	Level string `json:"level"`
	// File is repository-relative, cleaned, never with "..".
	File      string `json:"file"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	// Message is the tool's message, redacted and cut to MaxMessage runes.
	Message string `json:"message"`
	// HelpURI is the rule's documentation (https only), if the tool named one.
	HelpURI string `json:"help_uri,omitempty"`
	// Suppressed is set when the tool reported the result as suppressed
	// (an in-source or external suppression that was not rejected).
	Suppressed bool `json:"suppressed,omitempty"`
	// Fingerprint identifies the finding within its tool's run.
	Fingerprint string `json:"fingerprint"`

	// abs is an absolute path no base of the log resolved; Map places it.
	abs string
}

// Run is what one tool reported.
type Run struct {
	Tool     string    `json:"tool"`
	Version  string    `json:"version,omitempty"`
	Findings []Finding `json:"findings"`
	// Dropped counts results left out: no usable location (a URL, a path
	// outside the repository) or a kind that is not a problem (pass).
	Dropped int `json:"dropped,omitempty"`
}

// The subset of SARIF 2.1.0 that is read. Everything else, snippets and
// code flows included, is skipped by the decoder and never held.
type sarifLog struct {
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool struct {
		Driver struct {
			Name            string      `json:"name"`
			Version         string      `json:"version"`
			SemanticVersion string      `json:"semanticVersion"`
			Rules           []sarifRule `json:"rules"`
		} `json:"driver"`
	} `json:"tool"`
	OriginalURIBaseIDs map[string]artifactLocation `json:"originalUriBaseIds"`
	Artifacts          []struct {
		Location artifactLocation `json:"location"`
	} `json:"artifacts"`
	Results []sarifResult `json:"results"`
}

type sarifRule struct {
	ID               string          `json:"id"`
	HelpURI          string          `json:"helpUri"`
	ShortDescription *text           `json:"shortDescription"`
	MessageStrings   map[string]text `json:"messageStrings"`
	Default          *struct {
		Level string `json:"level"`
	} `json:"defaultConfiguration"`
	Properties properties `json:"properties"`
}

type text struct {
	Text string `json:"text"`
}

type properties struct {
	SecuritySeverity json.RawMessage `json:"security-severity"`
}

type artifactLocation struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId"`
	Index     *int   `json:"index"`
}

type sarifResult struct {
	RuleID    string `json:"ruleId"`
	RuleIndex *int   `json:"ruleIndex"`
	Rule      *struct {
		ID    string `json:"id"`
		Index *int   `json:"index"`
	} `json:"rule"`
	Kind    string `json:"kind"`
	Level   string `json:"level"`
	Message struct {
		Text      string   `json:"text"`
		ID        string   `json:"id"`
		Arguments []string `json:"arguments"`
	} `json:"message"`
	Locations []struct {
		Physical *struct {
			Artifact artifactLocation `json:"artifactLocation"`
			Region   *struct {
				StartLine int `json:"startLine"`
				EndLine   int `json:"endLine"`
			} `json:"region"`
		} `json:"physicalLocation"`
	} `json:"locations"`
	Suppressions []struct {
		Status string `json:"status"`
	} `json:"suppressions"`
	BaselineState string     `json:"baselineState"`
	Properties    properties `json:"properties"`
}

// Parse reads a SARIF 2.1.0 log. A run without a tool name is refused:
// every finding must name the tool that flagged it.
func Parse(r io.Reader) ([]Run, error) {
	lr := &io.LimitedReader{R: r, N: MaxLogBytes + 1}
	var lg sarifLog
	if err := json.NewDecoder(lr).Decode(&lg); err != nil {
		if lr.N <= 0 {
			return nil, ErrTooLarge
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalid, clipErr(err))
	}
	if lr.N <= 0 {
		return nil, ErrTooLarge
	}
	if lg.Version != "2.1.0" {
		return nil, fmt.Errorf("%w: version %q", ErrInvalid, clip(lg.Version, 20))
	}
	if len(lg.Runs) == 0 {
		return nil, fmt.Errorf("%w: no runs", ErrInvalid)
	}
	if len(lg.Runs) > MaxRuns {
		return nil, ErrTooLarge
	}
	var out []Run
	total := 0
	for i, sr := range lg.Runs {
		if len(sr.Tool.Driver.Rules) > maxRules {
			return nil, ErrTooLarge
		}
		total += len(sr.Results)
		if total > MaxFindings {
			return nil, ErrTooLarge
		}
		run, err := readRun(sr)
		if err != nil {
			return nil, fmt.Errorf("run %d: %w", i, err)
		}
		out = append(out, run)
	}
	return mergeRuns(out), nil
}

func readRun(sr sarifRun) (Run, error) {
	d := sr.Tool.Driver
	run := Run{Tool: Clean(d.Name, maxNameRunes), Version: Clean(cmp.Or(d.SemanticVersion, d.Version), maxNameRunes)}
	if run.Tool == "" {
		return Run{}, fmt.Errorf("%w: tool.driver.name is required", ErrInvalid)
	}
	rules := map[string]*sarifRule{}
	for i := range d.Rules {
		if _, dup := rules[d.Rules[i].ID]; !dup && d.Rules[i].ID != "" {
			rules[d.Rules[i].ID] = &d.Rules[i]
		}
	}
	bases := baseRoots(sr.OriginalURIBaseIDs)
	seen := map[string]bool{}
	for _, res := range sr.Results {
		f, ok := readResult(res, sr, d.Rules, rules, bases)
		if !ok {
			run.Dropped++
			continue
		}
		f.Fingerprint = fingerprint(run.Tool, f)
		if seen[f.Fingerprint] {
			continue
		}
		seen[f.Fingerprint] = true
		run.Findings = append(run.Findings, f)
	}
	return run, nil
}

func readResult(res sarifResult, sr sarifRun, list []sarifRule, rules map[string]*sarifRule, bases []string) (Finding, bool) {
	switch res.Kind {
	case "", "fail", "open", "review":
	default: // pass, notApplicable, informational
		return Finding{}, false
	}
	if res.BaselineState == "absent" {
		return Finding{}, false
	}
	ruleID := res.RuleID
	if ruleID == "" && res.Rule != nil {
		ruleID = res.Rule.ID
	}
	var rule *sarifRule
	idx := res.RuleIndex
	if idx == nil && res.Rule != nil {
		idx = res.Rule.Index
	}
	if idx != nil && *idx >= 0 && *idx < len(list) {
		rule = &list[*idx]
	} else if ruleID != "" {
		rule = rules[ruleID]
	}
	if ruleID == "" && rule != nil {
		ruleID = rule.ID
	}
	f := Finding{RuleID: Clean(ruleID, maxRuleRunes)}
	if f.RuleID == "" {
		return Finding{}, false
	}
	file, abs, start, end, ok := location(res, sr, bases)
	if !ok {
		return Finding{}, false
	}
	f.File, f.abs, f.StartLine, f.EndLine = file, abs, start, end

	f.Level = res.Level
	if f.Level == "" && rule != nil && rule.Default != nil {
		f.Level = rule.Default.Level
	}
	if !slices.Contains([]string{"error", "warning", "note", "none"}, f.Level) {
		f.Level = "warning" // SARIF's default
	}
	score, scored := securitySeverity(res.Properties)
	if !scored && rule != nil {
		score, scored = securitySeverity(rule.Properties)
	}
	f.Severity = Severity(f.Level, score, scored)
	f.Message = redact.Summary(message(res, rule, f.RuleID), MaxMessage)
	if rule != nil {
		f.HelpURI = helpURI(rule.HelpURI)
	}
	for _, s := range res.Suppressions {
		if s.Status != "rejected" && s.Status != "underReview" {
			f.Suppressed = true
		}
	}
	return f, true
}

// Severity maps a SARIF level, or GitHub's numeric security-severity when
// the tool gave one, onto CodeSynapse's severities.
func Severity(level string, score float64, scored bool) string {
	if scored {
		switch {
		case score >= 9:
			return "critical"
		case score >= 7:
			return "high"
		case score >= 4:
			return "medium"
		case score > 0:
			return "low"
		default:
			return "info"
		}
	}
	switch level {
	case "error":
		return "high"
	case "warning":
		return "medium"
	case "note":
		return "low"
	default:
		return "info"
	}
}

// securitySeverity reads the "security-severity" property, a number from 0
// to 10 given as a string (GitHub's convention) or a number.
func securitySeverity(p properties) (float64, bool) {
	raw := p.SecuritySeverity
	if len(raw) == 0 || len(raw) > 32 {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		s = string(raw)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 || v > 10 {
		return 0, false
	}
	return v, true
}

func message(res sarifResult, rule *sarifRule, ruleID string) string {
	if res.Message.Text != "" {
		return res.Message.Text
	}
	if rule != nil {
		if ms, ok := rule.MessageStrings[res.Message.ID]; ok && res.Message.ID != "" {
			msg := ms.Text
			for i, a := range res.Message.Arguments[:min(len(res.Message.Arguments), maxArgs)] {
				msg = strings.ReplaceAll(msg, "{"+strconv.Itoa(i)+"}", clip(a, MaxMessage))
			}
			return msg
		}
		if rule.ShortDescription != nil && rule.ShortDescription.Text != "" {
			return rule.ShortDescription.Text
		}
	}
	return ruleID
}

// location is the first physical location of a result: the cleaned
// repository-relative path, or an absolute one for Map to place.
func location(res sarifResult, sr sarifRun, bases []string) (file, abs string, start, end int, ok bool) {
	for _, loc := range res.Locations {
		if loc.Physical == nil {
			continue
		}
		al := loc.Physical.Artifact
		if al.URI == "" && al.Index != nil && *al.Index >= 0 && *al.Index < len(sr.Artifacts) {
			al = sr.Artifacts[*al.Index].Location
		}
		file, abs, ok = resolve(al.URI, bases)
		if !ok {
			return "", "", 0, 0, false
		}
		if rg := loc.Physical.Region; rg != nil && rg.StartLine > 0 && rg.StartLine <= maxLine {
			start, end = rg.StartLine, rg.EndLine
			if end < start || end > maxLine {
				end = start
			}
		}
		return file, abs, start, end, true
	}
	return "", "", 0, 0, false
}

// resolve turns an artifact URI into a repository-relative path. Relative
// URIs are relative to the repository root (SRCROOT); absolute ones are
// made relative to the log's file bases, else returned as abs for Map.
func resolve(uri string, bases []string) (file, abs string, ok bool) {
	p, isAbs, ok := filePath(uri)
	if !ok {
		return "", "", false
	}
	if !isAbs {
		file, ok = CleanPath(p)
		return file, "", ok
	}
	for _, b := range bases {
		if rest, found := strings.CutPrefix(p, b); found {
			file, ok = CleanPath(rest)
			return file, "", ok
		}
	}
	return "", p, true
}

// filePath decodes a file URI or plain path into a slash path and whether
// it is absolute. Other schemes (https, …) have no place in the checkout.
func filePath(uri string) (p string, isAbs, ok bool) {
	if uri == "" || len(uri) > maxPathLen || strings.ContainsAny(uri, "\x00\r\n") {
		return "", false, false
	}
	uri = strings.ReplaceAll(uri, "\\", "/")
	if i := strings.Index(uri, ":"); i > 1 && !strings.Contains(uri[:i], "/") {
		u, err := url.Parse(uri)
		if err != nil || !strings.EqualFold(u.Scheme, "file") || (u.Host != "" && u.Host != "localhost") {
			return "", false, false
		}
		uri = u.Path
		if !strings.HasPrefix(uri, "/") { // file:relative/x.go
			return uri, false, true
		}
	} else if dec, err := url.PathUnescape(uri); err == nil {
		uri = dec
	}
	if hasDrive(uri) {
		return "/" + uri, true, true
	}
	if hasDrive(strings.TrimPrefix(uri, "/")) || strings.HasPrefix(uri, "/") {
		return uri, true, true
	}
	return uri, false, true
}

func hasDrive(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0] >= 'a' && p[0] <= 'z' || p[0] >= 'A' && p[0] <= 'Z')
}

// baseRoots are the absolute file roots of originalUriBaseIds, longest
// first, each ending in "/".
func baseRoots(m map[string]artifactLocation) []string {
	var out []string
	for _, al := range m {
		p, isAbs, ok := filePath(al.URI)
		if !ok || !isAbs {
			continue
		}
		p = path.Clean(p)
		if p != "/" {
			p += "/"
		}
		if p != "/" {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), cmp.Compare(a, b)) })
	return slices.Compact(out)
}

// CleanPath returns p as a clean repository-relative path, or false when
// it leaves the repository or names nothing.
func CleanPath(p string) (string, bool) {
	if p == "" || len(p) > maxPathLen || strings.HasPrefix(p, "/") || hasDrive(p) {
		return "", false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return "", false
		}
	}
	c := path.Clean(p)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// Clean makes a name, version or rule ID from the log safe to show:
// printable, single-spaced, redacted and at most limit runes.
func Clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if !unicode.IsPrint(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, clip(s, 4*limit))
	return redact.Summary(s, limit)
}

// helpURI keeps a rule's documentation link when it is a plain https URL.
func helpURI(raw string) string {
	if raw == "" || len(raw) > maxHelpURI {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.ContainsAny(raw, " \t\r\n\"<>") {
		return ""
	}
	return u.String()
}

func fingerprint(tool string, f Finding) string {
	h := sha256.New()
	for _, part := range []string{tool, f.RuleID, f.File, f.abs, strconv.Itoa(f.StartLine), strconv.Itoa(f.EndLine), f.Message} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// mergeRuns joins runs of the same tool (case-insensitive), so a log that
// splits one tool's results keeps them together, and sorts by tool.
func mergeRuns(runs []Run) []Run {
	var out []Run
	for _, r := range runs {
		i := slices.IndexFunc(out, func(o Run) bool { return strings.EqualFold(o.Tool, r.Tool) })
		if i < 0 {
			out = append(out, r)
			continue
		}
		out[i].Dropped += r.Dropped
		seen := make(map[string]bool, len(out[i].Findings))
		for _, f := range out[i].Findings {
			seen[f.Fingerprint] = true
		}
		for _, f := range r.Findings {
			if !seen[f.Fingerprint] {
				seen[f.Fingerprint] = true
				out[i].Findings = append(out[i].Findings, f)
			}
		}
	}
	slices.SortFunc(out, func(a, b Run) int { return cmp.Compare(strings.ToLower(a.Tool), strings.ToLower(b.Tool)) })
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func clipErr(err error) string { return clip(err.Error(), 200) }
