package neo4jstore

import "github.com/parisnakitakejser/codesynapse/internal/model"

// toProps converts a node to Neo4j properties. Zero values are omitted so
// nodes stay small; fromProps restores them as zero values.
func toProps(n model.Node) map[string]any {
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
	setStr("language", n.Language)
	setStr("ecosystem", n.Ecosystem)
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

func fromProps(p map[string]any) model.Node {
	s := func(k string) string { v, _ := p[k].(string); return v }
	i := func(k string) int { v, _ := p[k].(int64); return int(v) }
	b := func(k string) bool { v, _ := p[k].(bool); return v }
	n := model.Node{
		ID: s("id"), Kind: model.NodeKind(s("kind")), Repo: s("repo"), Name: s("name"),
		QualifiedName: s("qualified_name"), Package: s("package"), File: s("file"),
		Signature: s("signature"), Summary: s("summary"), BodyHash: s("body_hash"),
		Time: s("time"), Head: s("head"), Root: s("root"), Branch: s("branch"), Metrics: s("metrics"), Bytes: i("bytes"),
		StartLine: i("start_line"), EndLine: i("end_line"), Complexity: i("complexity"),
		LOC: i("loc"), Level: i("level"), Churn: i("churn"),
		Exported: b("exported"), IsTest: b("is_test"),
		Severity: s("severity"), Rule: s("rule"), Version: s("version"), FixedVersion: s("fixed_version"),
		Reachability: s("reachability"), URL: s("url"), Indirect: b("indirect"),
		Liveness: s("liveness"), Cleanup: s("cleanup"), Language: s("language"), Ecosystem: s("ecosystem"),
		CWE: strs(p["cwe"]), Aliases: strs(p["aliases"]),
	}
	n.SecurityTags = strs(p["security_tags"])
	return n
}

// strs converts a Neo4j list property to []string (nil when absent).
func strs(v any) []string {
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
