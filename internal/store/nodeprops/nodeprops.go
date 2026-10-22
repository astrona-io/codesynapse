// Package nodeprops maps graph nodes to the flat property maps the graph
// databases store, shared by every store so a new node field cannot be
// saved by one and lost by another.
package nodeprops

import (
	"encoding/json"

	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// Lists are the properties holding string lists.
var Lists = []string{"cwe", "aliases", "security_tags"}

// To converts a node to properties. Zero values are omitted so nodes stay
// small; From restores them as zero values.
func To(n model.Node) map[string]any {
	p := map[string]any{"id": n.ID, "kind": string(n.Kind), "repo": n.Repo, "name": n.Name}
	setStr := func(k, v string) {
		if v != "" {
			p[k] = v
		}
	}
	setInt := func(k string, v int) {
		if v != 0 {
			p[k] = int64(v)
		}
	}
	setBool := func(k string, v bool) {
		if v {
			p[k] = true
		}
	}
	setStr("qualified_name", n.QualifiedName)
	setStr("package", n.Package)
	setStr("file", n.File)
	setStr("signature", n.Signature)
	setStr("summary", n.Summary)
	setStr("body_hash", n.BodyHash)
	setStr("clone_hash", n.CloneHash)
	setStr("via", n.Via)
	setInt("roots", n.Roots)
	setStr("clone_sig", n.CloneSig)
	setStr("duplicates", n.Duplicates)
	setStr("time", n.Time)
	setStr("head", n.Head)
	setStr("root", n.Root)
	setStr("branch", n.Branch)
	setStr("metrics", n.Metrics)
	setInt("bytes", n.Bytes)
	setInt("start_line", n.StartLine)
	setInt("end_line", n.EndLine)
	setInt("complexity", n.Complexity)
	setInt("loc", n.LOC)
	setInt("level", n.Level)
	setInt("churn", n.Churn)
	setBool("exported", n.Exported)
	setBool("is_test", n.IsTest)
	setStr("severity", n.Severity)
	setStr("rule", n.Rule)
	setStr("version", n.Version)
	setStr("fixed_version", n.FixedVersion)
	setStr("reachability", n.Reachability)
	setStr("url", n.URL)
	setBool("indirect", n.Indirect)
	setStr("liveness", n.Liveness)
	setStr("cleanup", n.Cleanup)
	setStr("licenses", n.Licenses)
	setStr("coverage", n.Coverage)
	setStr("language", n.Language)
	setStr("ecosystem", n.Ecosystem)
	setStr("license", n.License)
	setStr("license_source", n.LicenseSource)
	setBool("dev", n.Dev)
	if len(n.CWE) > 0 {
		p["cwe"] = n.CWE
	}
	if len(n.Aliases) > 0 {
		p["aliases"] = n.Aliases
	}
	if len(n.SecurityTags) > 0 {
		p["security_tags"] = n.SecurityTags
	}
	return p
}

// From restores a node. Integers may arrive as int64 (Neo4j), float64 or
// json.Number (JSON), lists as []any or []string.
func From(p map[string]any) model.Node {
	s := func(k string) string { v, _ := p[k].(string); return v }
	i := func(k string) int { return Int(p[k]) }
	b := func(k string) bool { v, _ := p[k].(bool); return v }
	n := model.Node{
		ID: s("id"), Kind: model.NodeKind(s("kind")), Repo: s("repo"), Name: s("name"),
		QualifiedName: s("qualified_name"), Package: s("package"), File: s("file"),
		Signature: s("signature"), Summary: s("summary"), BodyHash: s("body_hash"), CloneHash: s("clone_hash"), Via: s("via"), Roots: i("roots"), CloneSig: s("clone_sig"), Duplicates: s("duplicates"),
		Time: s("time"), Head: s("head"), Root: s("root"), Branch: s("branch"), Metrics: s("metrics"), Bytes: i("bytes"),
		StartLine: i("start_line"), EndLine: i("end_line"), Complexity: i("complexity"),
		LOC: i("loc"), Level: i("level"), Churn: i("churn"),
		Exported: b("exported"), IsTest: b("is_test"),
		Severity: s("severity"), Rule: s("rule"), Version: s("version"), FixedVersion: s("fixed_version"),
		Reachability: s("reachability"), URL: s("url"), Indirect: b("indirect"),
		Liveness: s("liveness"), Cleanup: s("cleanup"), Licenses: s("licenses"), Coverage: s("coverage"), Language: s("language"), Ecosystem: s("ecosystem"),
		License: s("license"), LicenseSource: s("license_source"), Dev: b("dev"),
		CWE: Strs(p["cwe"]), Aliases: Strs(p["aliases"]),
	}
	n.SecurityTags = Strs(p["security_tags"])
	return n
}

// Int reads an integer property in any of the forms the drivers return.
func Int(v any) int {
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// Strs converts a list property to []string (nil when absent).
func Strs(v any) []string {
	if ss, ok := v.([]string); ok && len(ss) > 0 {
		return ss
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
