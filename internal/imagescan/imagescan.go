// Package imagescan reads the JSON report of a container image scanner (Trivy
// or Grype) into bounded findings. Reports come from CI and are untrusted:
// the parser caps size and counts, drops malformed entries and keeps only
// package names, versions, advisory IDs, severities and short titles.
package imagescan

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Limits on what one report may hold.
const (
	MaxReportBytes = 64 << 20
	MaxFindings    = 50_000
	maxFieldLen    = 256
)

// ErrTooLarge reports a report over the limits above.
var ErrTooLarge = errors.New("image scan report exceeds the size limits")

// Finding is one advisory affecting one package of one image.
type Finding struct {
	// Image is the scanned artifact ("ghcr.io/org/api:1.2.3").
	Image   string `json:"image"`
	Package string `json:"package"`
	Version string `json:"version"`
	// ID is the advisory ID, normally a CVE.
	ID       string `json:"id"`
	Severity string `json:"severity"` // critical, high, medium, low or unknown
	Fixed    string `json:"fixed,omitempty"`
	Title    string `json:"title,omitempty"`
}

var advisoryID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,63}$`)

// Parse reads a Trivy (`trivy image -f json`) or Grype (`grype -o json`)
// report, whichever the content is. Findings are de-duplicated and sorted so
// the same report always yields the same list.
func Parse(r io.Reader) ([]Finding, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxReportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxReportBytes {
		return nil, ErrTooLarge
	}
	var probe struct {
		Results *json.RawMessage `json:"Results"`
		Matches *json.RawMessage `json:"matches"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("image scan report: %w", err)
	}
	var out []Finding
	switch {
	case probe.Matches != nil:
		out, err = parseGrype(raw)
	case probe.Results != nil || bytes.Contains(raw, []byte(`"ArtifactName"`)):
		out, err = parseTrivy(raw)
	default:
		return nil, errors.New("image scan report: neither a Trivy nor a Grype report")
	}
	if err != nil {
		return nil, err
	}
	return normalise(out)
}

func parseTrivy(raw []byte) ([]Finding, error) {
	var doc struct {
		ArtifactName string
		Results      []struct {
			Vulnerabilities []struct {
				VulnerabilityID  string
				PkgName          string
				InstalledVersion string
				FixedVersion     string
				Severity         string
				Title            string
			}
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("trivy report: %w", err)
	}
	var out []Finding
	for _, res := range doc.Results {
		for _, v := range res.Vulnerabilities {
			out = append(out, Finding{Image: doc.ArtifactName, Package: v.PkgName, Version: v.InstalledVersion,
				ID: v.VulnerabilityID, Severity: v.Severity, Fixed: v.FixedVersion, Title: v.Title})
			if len(out) > MaxFindings*2 {
				return nil, ErrTooLarge
			}
		}
	}
	return out, nil
}

func parseGrype(raw []byte) ([]Finding, error) {
	var doc struct {
		Source struct {
			Target json.RawMessage
		}
		Matches []struct {
			Vulnerability struct {
				ID       string
				Severity string
				Fix      struct{ Versions []string }
			}
			Artifact struct{ Name, Version string }
		}
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("grype report: %w", err)
	}
	image := grypeImage(doc.Source.Target)
	var out []Finding
	for _, m := range doc.Matches {
		f := Finding{Image: image, Package: m.Artifact.Name, Version: m.Artifact.Version,
			ID: m.Vulnerability.ID, Severity: m.Vulnerability.Severity}
		if len(m.Vulnerability.Fix.Versions) > 0 {
			f.Fixed = strings.Join(m.Vulnerability.Fix.Versions, ", ")
		}
		out = append(out, f)
		if len(out) > MaxFindings*2 {
			return nil, ErrTooLarge
		}
	}
	return out, nil
}

// grypeImage is the image name: newer reports carry an object with userInput,
// older ones a plain string.
func grypeImage(target json.RawMessage) string {
	var s string
	if json.Unmarshal(target, &s) == nil {
		return s
	}
	var o struct{ UserInput, ImageID string }
	if json.Unmarshal(target, &o) == nil {
		if o.UserInput != "" {
			return o.UserInput
		}
		return o.ImageID
	}
	return ""
}

func normalise(in []Finding) ([]Finding, error) {
	seen := map[Finding]bool{}
	var out []Finding
	for _, f := range in {
		f.Image, f.Package, f.Version = clip(f.Image), clip(f.Package), clip(f.Version)
		f.Fixed, f.Title = clip(f.Fixed), clip(f.Title)
		f.ID = strings.TrimSpace(f.ID)
		f.Severity = severity(f.Severity)
		if f.Package == "" || !advisoryID.MatchString(f.ID) {
			continue
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) > MaxFindings {
		return nil, ErrTooLarge
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Image != b.Image {
			return a.Image < b.Image
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.ID < b.ID
	})
	return out, nil
}

func severity(s string) string {
	switch s = strings.ToLower(strings.TrimSpace(s)); s {
	case "critical", "high", "medium", "low":
		return s
	}
	return "unknown"
}

// clip trims and bounds a field, cutting on a rune boundary.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxFieldLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxFieldLen], "")
}
