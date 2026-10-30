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

	// abs is an absolute path no base of the log resolved; Place places it.
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

// The log is read as a stream of JSON tokens, one result, rule or artifact
// at a time, so what is held grows with what is kept, never with what the
// log repeats: decoding it into typed structs first let a few KiB of gzip
// ("results":[{},{},…]) allocate gigabytes. Only the fields below are read;
// everything else, snippets and code flows included, is skipped token by
// token, and every string is cut to its limit as soon as it is read.

// rawRule is what is read of a rule.
type rawRule struct {
	id, helpURI, short, level string
	// messages are the rule's message templates (messageStrings).
	messages map[string]string
	score    float64
	scored   bool
}

// rawResult is what is read of a result, before the rules and artifacts
// it refers to (which may come later in the run) are known.
type rawResult struct {
	ruleID, ruleRefID   string
	ruleIndex, refIndex *int
	kind, level         string
	text, msgID         string
	args                []string
	loc                 rawLocation
	hasLoc              bool
	suppressed, absent  bool
	score               float64
	scored              bool
}

type rawLocation struct {
	uri        string
	index      *int
	start, end int
}

type rawRun struct {
	name, version, semver string
	rules                 []rawRule
	bases                 []string
	artifacts             []string
	results               []rawResult
}

// Limits of the reader beyond those on what is kept.
const (
	// maxRawMessage bounds, in bytes, a message, template or description as
	// read, before arguments are filled in and it is redacted and cut to
	// MaxMessage runes.
	maxRawMessage = 2048
	// maxMessageStrings bounds the message templates kept per log.
	maxMessageStrings = 20_000
	// maxArtifacts bounds the artifacts of one log.
	maxArtifacts = 50_000
	// maxBases bounds the originalUriBaseIds kept per run.
	maxBases = 64
	// maxDepth bounds the nesting of skipped values: the decoder keeps a
	// stack entry per open array or object.
	maxDepth = 64
	maxShort = 32
)

// parser counts across the whole log, so splitting it into runs gains
// nothing.
type parser struct {
	dec                                   *json.Decoder
	rules, artifacts, results, msgStrings int
}

// Parse reads a SARIF 2.1.0 log. A run without a tool name is refused:
// every finding must name the tool that flagged it.
func Parse(r io.Reader) ([]Run, error) {
	lr := &io.LimitedReader{R: r, N: MaxLogBytes + 1}
	p := &parser{dec: json.NewDecoder(lr)}
	p.dec.UseNumber()
	var version string
	var runs []rawRun
	err := p.object(func(k string) error {
		var err error
		switch k {
		case "version":
			version, err = p.str(maxShort)
		case "runs":
			err = p.array(func() error {
				if len(runs) >= MaxRuns {
					return ErrTooLarge
				}
				run, err := p.run()
				runs = append(runs, run)
				return err
			})
		default:
			err = p.skip()
		}
		return err
	})
	if lr.N <= 0 {
		return nil, ErrTooLarge
	}
	if err != nil {
		if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrInvalid) {
			return nil, err
		}
		// %w keeps a reader's error, such as http.MaxBytesError, for the caller.
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if version != "2.1.0" {
		return nil, fmt.Errorf("%w: version %q", ErrInvalid, version)
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("%w: no runs", ErrInvalid)
	}
	out := make([]Run, 0, len(runs))
	for i, rr := range runs {
		run, err := readRun(rr)
		if err != nil {
			return nil, fmt.Errorf("run %d: %w", i, err)
		}
		out = append(out, run)
	}
	return mergeRuns(out), nil
}

func (p *parser) run() (rawRun, error) {
	var rr rawRun
	err := p.object(func(k string) error {
		switch k {
		case "tool":
			return p.object(func(k string) error {
				if k != "driver" {
					return p.skip()
				}
				return p.driver(&rr)
			})
		case "originalUriBaseIds":
			return p.object(func(string) error {
				loc, err := p.artifactLocation()
				if err == nil && loc.uri != "" && len(rr.bases) < maxBases {
					rr.bases = append(rr.bases, loc.uri)
				}
				return err
			})
		case "artifacts":
			return p.array(func() error {
				if p.artifacts >= maxArtifacts {
					return ErrTooLarge
				}
				p.artifacts++
				var uri string
				err := p.object(func(k string) error {
					if k != "location" {
						return p.skip()
					}
					loc, err := p.artifactLocation()
					uri = loc.uri
					return err
				})
				rr.artifacts = append(rr.artifacts, uri)
				return err
			})
		case "results":
			return p.array(func() error {
				if p.results >= MaxFindings {
					return ErrTooLarge
				}
				p.results++
				res, err := p.result()
				rr.results = append(rr.results, res)
				return err
			})
		default:
			return p.skip()
		}
	})
	return rr, err
}

func (p *parser) driver(rr *rawRun) error {
	return p.object(func(k string) error {
		var err error
		switch k {
		case "name":
			rr.name, err = p.str(4 * maxNameRunes)
		case "version":
			rr.version, err = p.str(4 * maxNameRunes)
		case "semanticVersion":
			rr.semver, err = p.str(4 * maxNameRunes)
		case "rules":
			err = p.array(func() error {
				if p.rules >= maxRules {
					return ErrTooLarge
				}
				p.rules++
				rule, err := p.rule()
				rr.rules = append(rr.rules, rule)
				return err
			})
		default:
			err = p.skip()
		}
		return err
	})
}

func (p *parser) rule() (rawRule, error) {
	var rule rawRule
	err := p.object(func(k string) error {
		var err error
		switch k {
		case "id":
			rule.id, err = p.str(4 * maxRuleRunes)
		case "helpUri":
			rule.helpURI, err = p.str(maxHelpURI + 1)
		case "shortDescription":
			rule.short, err = p.text()
		case "messageStrings":
			err = p.object(func(id string) error {
				if p.msgStrings >= maxMessageStrings {
					return p.skip()
				}
				p.msgStrings++
				t, err := p.text()
				if rule.messages == nil {
					rule.messages = map[string]string{}
				}
				if _, dup := rule.messages[id]; !dup {
					rule.messages[strings.Clone(clip(id, 4*maxRuleRunes))] = t
				}
				return err
			})
		case "defaultConfiguration":
			err = p.object(func(k string) error {
				if k != "level" {
					return p.skip()
				}
				var err error
				rule.level, err = p.str(maxShort)
				return err
			})
		case "properties":
			rule.score, rule.scored, err = p.properties()
		default:
			err = p.skip()
		}
		return err
	})
	return rule, err
}

func (p *parser) result() (rawResult, error) {
	var res rawResult
	err := p.object(func(k string) error {
		var err error
		switch k {
		case "ruleId":
			res.ruleID, err = p.str(4 * maxRuleRunes)
		case "ruleIndex":
			res.ruleIndex, err = p.integer()
		case "rule":
			err = p.object(func(k string) error {
				var err error
				switch k {
				case "id":
					res.ruleRefID, err = p.str(4 * maxRuleRunes)
				case "index":
					res.refIndex, err = p.integer()
				default:
					err = p.skip()
				}
				return err
			})
		case "kind":
			res.kind, err = p.str(maxShort)
		case "level":
			res.level, err = p.str(maxShort)
		case "message":
			err = p.message(&res)
		case "locations":
			err = p.array(func() error {
				if res.hasLoc {
					return p.skip()
				}
				loc, found, err := p.location()
				res.loc, res.hasLoc = loc, found
				return err
			})
		case "suppressions":
			err = p.array(func() error {
				var status string
				err := p.object(func(k string) error {
					if k != "status" {
						return p.skip()
					}
					var err error
					status, err = p.str(maxShort)
					return err
				})
				if status != "rejected" && status != "underReview" {
					res.suppressed = true
				}
				return err
			})
		case "baselineState":
			var state string
			state, err = p.str(maxShort)
			res.absent = state == "absent"
		case "properties":
			res.score, res.scored, err = p.properties()
		default:
			err = p.skip()
		}
		return err
	})
	return res, err
}

func (p *parser) message(res *rawResult) error {
	return p.object(func(k string) error {
		var err error
		switch k {
		case "text":
			var s string
			s, err = p.str(maxRawMessage + 1)
			res.text = clipWords(s, maxRawMessage)
		case "id":
			res.msgID, err = p.str(4 * maxRuleRunes)
		case "arguments":
			err = p.array(func() error {
				if len(res.args) >= maxArgs {
					return p.skip()
				}
				a, err := p.str(MaxMessage)
				res.args = append(res.args, a)
				return err
			})
		default:
			err = p.skip()
		}
		return err
	})
}

// location reads one location; found is false when it has no physical one.
func (p *parser) location() (rawLocation, bool, error) {
	var loc rawLocation
	found := false
	err := p.object(func(k string) error {
		if k != "physicalLocation" {
			return p.skip()
		}
		present, err := p.objectOK(func(k string) error {
			switch k {
			case "artifactLocation":
				al, err := p.artifactLocation()
				loc.uri, loc.index = al.uri, al.index
				return err
			case "region":
				return p.object(func(k string) error {
					var n *int
					var err error
					switch k {
					case "startLine":
						n, err = p.integer()
						loc.start = deref(n)
					case "endLine":
						n, err = p.integer()
						loc.end = deref(n)
					default:
						err = p.skip()
					}
					return err
				})
			default:
				return p.skip()
			}
		})
		found = found || present
		return err
	})
	return loc, found, err
}

func (p *parser) artifactLocation() (rawLocation, error) {
	var loc rawLocation
	err := p.object(func(k string) error {
		var err error
		switch k {
		case "uri":
			// One byte over the limit, so filePath refuses a longer one.
			loc.uri, err = p.str(maxPathLen + 1)
		case "index":
			loc.index, err = p.integer()
		default:
			err = p.skip()
		}
		return err
	})
	return loc, err
}

// text reads a multiformatMessageString's text.
func (p *parser) text() (string, error) {
	var t string
	err := p.object(func(k string) error {
		if k != "text" {
			return p.skip()
		}
		s, err := p.str(maxRawMessage + 1)
		t = clipWords(s, maxRawMessage)
		return err
	})
	return t, err
}

// properties reads the "security-severity" property, a number from 0 to
// 10 given as a string (GitHub's convention) or a number.
func (p *parser) properties() (score float64, scored bool, err error) {
	err = p.object(func(k string) error {
		if k != "security-severity" {
			return p.skip()
		}
		tok, ok, err := p.scalar()
		if err != nil || !ok {
			return err
		}
		var s string
		switch v := tok.(type) {
		case string:
			s = v
		case json.Number:
			s = v.String()
		}
		if len(s) == 0 || len(s) > 32 {
			return nil
		}
		v, perr := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if perr == nil && v >= 0 && v <= 10 {
			score, scored = v, true
		}
		return nil
	})
	return score, scored, err
}

// object reads an object, calling fn with each key; fn reads the value.
// null reads as an empty object.
func (p *parser) object(fn func(key string) error) error {
	_, err := p.objectOK(fn)
	return err
}

// objectOK is object that also says whether there was one (not null).
func (p *parser) objectOK(fn func(key string) error) (bool, error) {
	tok, err := p.dec.Token()
	if err != nil {
		return false, err
	}
	if tok == nil {
		return false, nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return false, fmt.Errorf("%w: expected an object", ErrInvalid)
	}
	for p.dec.More() {
		tok, err := p.dec.Token()
		if err != nil {
			return true, err
		}
		key, ok := tok.(string)
		if !ok {
			return true, fmt.Errorf("%w: expected a key", ErrInvalid)
		}
		if err := fn(key); err != nil {
			return true, err
		}
	}
	_, err = p.dec.Token() // }
	return true, err
}

// array reads an array, calling fn for each element; fn reads it. null
// reads as an empty array.
func (p *parser) array(fn func() error) error {
	tok, err := p.dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("%w: expected an array", ErrInvalid)
	}
	for p.dec.More() {
		if err := fn(); err != nil {
			return err
		}
	}
	_, err = p.dec.Token() // ]
	return err
}

// scalar reads one value: a string, number, bool or null (ok), or an
// array or object, which it skips (not ok).
func (p *parser) scalar() (json.Token, bool, error) {
	tok, err := p.dec.Token()
	if err != nil {
		return nil, false, err
	}
	if d, isDelim := tok.(json.Delim); isDelim {
		return nil, false, p.skipOpen(d)
	}
	return tok, true, nil
}

// str reads a string cut to limit bytes; any other value reads as "".
func (p *parser) str(limit int) (string, error) {
	tok, ok, err := p.scalar()
	if err != nil || !ok {
		return "", err
	}
	s, _ := tok.(string)
	if len(s) > limit {
		// A copy, so the cut never holds the whole token in memory.
		s = strings.Clone(clip(s, limit))
	}
	return s, nil
}

// integer reads an integer; any other value reads as absent.
func (p *parser) integer() (*int, error) {
	tok, ok, err := p.scalar()
	if err != nil || !ok {
		return nil, err
	}
	num, isNum := tok.(json.Number)
	if !isNum || len(num) > 12 {
		return nil, nil
	}
	if n, perr := strconv.Atoi(num.String()); perr == nil {
		return &n, nil
	}
	return nil, nil
}

// skip reads past one value of any shape without keeping it.
func (p *parser) skip() error {
	_, _, err := p.scalar()
	return err
}

// skipOpen reads to the end of the array or object opened by d.
func (p *parser) skipOpen(d json.Delim) error {
	if d != '{' && d != '[' {
		return fmt.Errorf("%w: unexpected %v", ErrInvalid, d)
	}
	for depth := 1; depth > 0; {
		tok, err := p.dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
			if depth > maxDepth {
				return fmt.Errorf("%w: nested more than %d deep", ErrInvalid, maxDepth)
			}
		}
	}
	return nil
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

func readRun(rr rawRun) (Run, error) {
	run := Run{Tool: Clean(rr.name, maxNameRunes), Version: Clean(cmp.Or(rr.semver, rr.version), maxNameRunes)}
	if run.Tool == "" {
		return Run{}, fmt.Errorf("%w: tool.driver.name is required", ErrInvalid)
	}
	rules := map[string]*rawRule{}
	for i := range rr.rules {
		if _, dup := rules[rr.rules[i].id]; !dup && rr.rules[i].id != "" {
			rules[rr.rules[i].id] = &rr.rules[i]
		}
	}
	bases := baseRoots(rr.bases)
	seen := map[string]bool{}
	for _, res := range rr.results {
		f, ok := readResult(res, rr, rules, bases)
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

func readResult(res rawResult, rr rawRun, rules map[string]*rawRule, bases []string) (Finding, bool) {
	switch res.kind {
	case "", "fail", "open", "review":
	default: // pass, notApplicable, informational
		return Finding{}, false
	}
	if res.absent {
		return Finding{}, false
	}
	ruleID := cmp.Or(res.ruleID, res.ruleRefID)
	var rule *rawRule
	idx := res.ruleIndex
	if idx == nil {
		idx = res.refIndex
	}
	if idx != nil && *idx >= 0 && *idx < len(rr.rules) {
		rule = &rr.rules[*idx]
	} else if ruleID != "" {
		rule = rules[ruleID]
	}
	if ruleID == "" && rule != nil {
		ruleID = rule.id
	}
	f := Finding{RuleID: Clean(ruleID, maxRuleRunes)}
	if f.RuleID == "" || !res.hasLoc {
		return Finding{}, false
	}
	file, abs, start, end, ok := location(res.loc, rr.artifacts, bases)
	if !ok {
		return Finding{}, false
	}
	f.File, f.abs, f.StartLine, f.EndLine = file, abs, start, end

	f.Level = res.level
	if f.Level == "" && rule != nil {
		f.Level = rule.level
	}
	if !slices.Contains([]string{"error", "warning", "note", "none"}, f.Level) {
		f.Level = "warning" // SARIF's default
	}
	score, scored := res.score, res.scored
	if !scored && rule != nil {
		score, scored = rule.score, rule.scored
	}
	f.Severity = Severity(f.Level, score, scored)
	f.Message = redact.Summary(message(res, rule, f.RuleID), MaxMessage)
	if rule != nil {
		f.HelpURI = helpURI(rule.helpURI)
	}
	f.Suppressed = res.suppressed
	return f, true
}

// message is the result's text, else its rule's template with the
// arguments filled in, else the rule's short description. It is at most
// maxRawMessage bytes, so redacting it costs little.
func message(res rawResult, rule *rawRule, ruleID string) string {
	if res.text != "" {
		return res.text
	}
	if rule != nil {
		if tmpl, ok := rule.messages[res.msgID]; ok && res.msgID != "" {
			return fill(tmpl, res.args)
		}
		if rule.short != "" {
			return rule.short
		}
	}
	return ruleID
}

// fill puts arguments into a template's {0}…{9} in one pass, stopping once
// the message passes maxRawMessage bytes; arguments are not filled again.
func fill(tmpl string, args []string) string {
	var b strings.Builder
	for i := 0; i < len(tmpl) && b.Len() <= maxRawMessage; {
		if tmpl[i] == '{' && i+2 < len(tmpl) && tmpl[i+2] == '}' && tmpl[i+1] >= '0' && tmpl[i+1] <= '9' {
			if n := int(tmpl[i+1] - '0'); n < len(args) {
				b.WriteString(args[n])
				i += 3
				continue
			}
		}
		b.WriteByte(tmpl[i])
		i++
	}
	return clipWords(b.String(), maxRawMessage)
}

// clipWords cuts s to at most n bytes, dropping a word the cut would split
// so a partial secret cannot slip past redaction.
func clipWords(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = clip(s, n)
	if i := strings.LastIndexAny(s, " \t\r\n"); i > 0 {
		s = s[:i]
	}
	return s
}

// location is a result's first physical location: the cleaned
// repository-relative path, or an absolute one for Place.
func location(loc rawLocation, artifacts []string, bases []string) (file, abs string, start, end int, ok bool) {
	uri := loc.uri
	if uri == "" && loc.index != nil && *loc.index >= 0 && *loc.index < len(artifacts) {
		uri = artifacts[*loc.index]
	}
	file, abs, ok = resolve(uri, bases)
	if !ok {
		return "", "", 0, 0, false
	}
	if loc.start > 0 && loc.start <= maxLine {
		start, end = loc.start, loc.end
		if end < start || end > maxLine {
			end = start
		}
	}
	return file, abs, start, end, true
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
func baseRoots(uris []string) []string {
	var out []string
	for _, uri := range uris {
		p, isAbs, ok := filePath(uri)
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
	// Cleaning can make a drive ("./C:" becomes "C:").
	if c == "." || c == ".." || strings.HasPrefix(c, "../") || hasDrive(c) {
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
