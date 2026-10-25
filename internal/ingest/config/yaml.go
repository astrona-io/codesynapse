// Package config indexes YAML files and Go templates (including Helm charts)
// and runs security rules over Kubernetes manifests, compose files,
// GitHub Actions workflows and dependency update automation (Renovate,
// Dependabot).
//
// Only structure is stored: file nodes, entries with line ranges (a
// Kubernetes resource or a key path), template define blocks and the edges
// between them. Values are never stored, since YAML commonly holds secrets.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
	"github.com/astrona-io/codesynapse/rules"
)

// maxKeyDepth bounds key-path entries for plain YAML; values files of Helm
// charts go deeper so templates can link to specific values.
const (
	maxKeyDepth       = 2
	maxValuesKeyDepth = 4
)

type builder struct {
	policy *repoconfig.Policy
	repo   string
	g      *model.Graph
}

func (b *builder) id(kind model.NodeKind, local string) string {
	return model.NodeID(b.repo, kind, local)
}

// yamlFile indexes one YAML file. valuesFile marks a Helm values.yaml.
func (b *builder) yamlFile(rel string, content []byte, valuesFile bool) {
	lines := bytes.Count(content, []byte("\n")) + 1
	fileID := b.id(model.KindConfig, rel)
	file := b.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindConfig, Repo: b.repo, Name: baseName(rel), File: rel,
		StartLine: 1, EndLine: lines, LOC: lines, Bytes: len(content),
	})
	b.g.AddEdge(model.Edge{From: model.NodeID(b.repo, model.KindRepository, b.repo), To: fileID, Kind: model.EdgeContains})

	dec := yaml.NewDecoder(bytes.NewReader(content))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			file.Summary = "unparseable YAML: " + redact.Summary(err.Error(), 120)
			return
		}
		docs = append(docs, &doc)
	}

	var kinds []string
	for i, doc := range docs {
		root := docRoot(doc)
		if root == nil || root.Kind != yaml.MappingNode {
			continue
		}
		end := lines
		if i+1 < len(docs) {
			end = max(root.Line, docs[i+1].Line-1)
		}
		if res, ok := k8sResource(root); ok && b.policy.KubernetesAllowed(rel) {
			entryID := b.id(model.KindConfigEntry, fmt.Sprintf("%s#%s", rel, res))
			b.g.AddNode(model.Node{
				ID: entryID, Kind: model.KindConfigEntry, Repo: b.repo, Name: res, QualifiedName: res, File: rel,
				StartLine: root.Line, EndLine: end, Signature: scalar(mapGet(root, "apiVersion")),
			})
			b.g.AddEdge(model.Edge{From: fileID, To: entryID, Kind: model.EdgeDefines})
			kinds = append(kinds, res)
			b.k8sRules(rel, entryID, root)
			continue
		}
		depth := maxKeyDepth
		if valuesFile {
			depth = maxValuesKeyDepth
		}
		b.keys(rel, fileID, root, "", depth, end)
	}

	b.genericRules(rel, fileID, docs)
	if isWorkflow(rel) {
		b.workflowRules(rel, fileID, docs)
	}
	if isCompose(rel) {
		b.composeRules(rel, fileID, docs)
	}

	switch {
	case len(kinds) > 0:
		file.Summary = redact.Summary("Kubernetes: "+strings.Join(kinds, ", "), 240)
	case valuesFile:
		file.Summary = "Helm values"
	default:
		var top []string
		for _, doc := range docs {
			if root := docRoot(doc); root != nil && root.Kind == yaml.MappingNode {
				for i := 0; i < len(root.Content); i += 2 {
					top = append(top, root.Content[i].Value)
				}
			}
		}
		if len(top) > 0 {
			file.Summary = redact.Summary("keys: "+strings.Join(top, ", "), 240)
		}
	}
}

// keys adds key-path entries ("services.api") down to depth.
func (b *builder) keys(rel, parentID string, m *yaml.Node, prefix string, depth, end int) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		path := k.Value
		if prefix != "" {
			path = prefix + "." + k.Value
		}
		entryEnd := end
		if i+2 < len(m.Content) {
			entryEnd = max(k.Line, m.Content[i+2].Line-1)
		}
		id := b.id(model.KindConfigEntry, rel+"#"+path)
		b.g.AddNode(model.Node{
			ID: id, Kind: model.KindConfigEntry, Repo: b.repo, Name: path, QualifiedName: path, File: rel,
			StartLine: k.Line, EndLine: entryEnd, Signature: yamlKind(v),
		})
		b.g.AddEdge(model.Edge{From: parentID, To: id, Kind: model.EdgeDefines})
		if depth > 1 && v.Kind == yaml.MappingNode {
			b.keys(rel, id, v, path, depth-1, entryEnd)
		}
	}
}

func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return nil
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalar(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

func yamlKind(n *yaml.Node) string {
	switch n.Kind {
	case yaml.MappingNode:
		return "map"
	case yaml.SequenceNode:
		return "list"
	case yaml.AliasNode:
		return "alias"
	}
	return "scalar"
}

// k8sResource returns "Kind/name" for Kubernetes-shaped documents.
func k8sResource(root *yaml.Node) (string, bool) {
	kind, api := scalar(mapGet(root, "kind")), scalar(mapGet(root, "apiVersion"))
	if kind == "" || api == "" {
		return "", false
	}
	name := scalar(mapGet(mapGet(root, "metadata"), "name"))
	if name == "" {
		name = "(unnamed)"
	}
	return kind + "/" + name, true
}

func baseName(rel string) string {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[i+1:]
	}
	return rel
}

func isWorkflow(rel string) bool {
	return strings.HasPrefix(rel, ".github/workflows/") || strings.Contains(rel, "/.github/workflows/")
}

func isCompose(rel string) bool {
	b := baseName(rel)
	return strings.HasPrefix(b, "docker-compose") || strings.HasPrefix(b, "compose.") || b == "compose.yaml" || b == "compose.yml"
}

// --- security rules ------------------------------------------------------

// RuleIDs are the rules this detector reports; see rules/config/<id>.yaml.
var RuleIDs = []string{
	"k8s-privileged",
	"k8s-privilege-escalation",
	"k8s-host-namespace",
	"k8s-host-path",
	"k8s-run-as-root",
	"k8s-dangerous-capability",
	"k8s-image-unpinned",
	"k8s-secret-in-repo",
	"yaml-hardcoded-secret",
	"gha-script-injection",
	"gha-pr-target-checkout",
	"gha-unpinned-action",
	"compose-privileged",
	"dependency-updates-missing",
	"renovate-config-invalid",
	"renovate-disabled",
	"renovate-vulnerability-alerts-off",
	"renovate-automerge-no-release-age",
	"renovate-no-digest-pinning",
	"dockerfile-root-user",
	"dockerfile-add-url",
	"dockerfile-curl-pipe-shell",
	"dockerfile-image-unpinned",
	"dockerfile-secret-in-env",
	"tf-open-ingress",
	"tf-public-storage",
	"tf-public-database",
	"tf-unencrypted-storage",
	"tf-secret-literal",
}

func (b *builder) report(rel, ownerID, ruleID string, line int, detail string) {
	b.reportIn(rel, ownerID, ruleID, line, detail, "")
}

// reportIn reports a finding whose language the file name does not tell
// (the fix examples to show are chosen by it).
func (b *builder) reportIn(rel, ownerID, ruleID string, line int, detail, language string) {
	r := rules.MustGet(ruleID)
	id := b.id(model.KindFinding, fmt.Sprintf("%s:%d:%s", rel, line, ruleID))
	b.g.AddNode(model.Node{
		ID: id, Kind: model.KindFinding, Repo: b.repo, Name: r.Title, Rule: r.ID, Severity: r.Severity, CWE: r.CWE,
		File: rel, StartLine: line, EndLine: line, Language: language, Summary: redact.Summary(detail+". "+r.Fix, 300),
	})
	b.g.AddEdge(model.Edge{From: ownerID, To: id, Kind: model.EdgeHasFinding})
}

// walk calls fn for every mapping key/value pair below n.
func walk(n *yaml.Node, fn func(key, val *yaml.Node)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walk(c, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			fn(n.Content[i], n.Content[i+1])
			walk(n.Content[i+1], fn)
		}
	}
}

var dangerousCaps = map[string]bool{"SYS_ADMIN": true, "NET_ADMIN": true, "ALL": true, "SYS_PTRACE": true, "SYS_MODULE": true, "DAC_READ_SEARCH": true}

func (b *builder) k8sRules(rel, entryID string, root *yaml.Node) {
	kind := scalar(mapGet(root, "kind"))
	switch kind {
	case "CustomResourceDefinition":
		return // its schema describes fields such as hostPath; it sets nothing
	case "Secret":
		for _, field := range []string{"data", "stringData"} {
			d := mapGet(root, field)
			if d == nil || d.Kind != yaml.MappingNode {
				continue
			}
			// Only literal values count: templated or empty ones are filled in
			// at deploy time.
			literal := 0
			for i := 1; i < len(d.Content); i += 2 {
				if v := d.Content[i]; v.Kind == yaml.ScalarNode && strings.TrimSpace(v.Value) != "" && !templated(v.Value) {
					literal++
				}
			}
			if literal > 0 {
				b.report(rel, entryID, "k8s-secret-in-repo", d.Line, fmt.Sprintf("Secret %s has %d literal %s keys", scalar(mapGet(mapGet(root, "metadata"), "name")), literal, field))
			}
		}
		return
	}
	walk(root, func(k, v *yaml.Node) {
		switch k.Value {
		case "privileged":
			if v.Value == "true" {
				b.report(rel, entryID, "k8s-privileged", k.Line, "privileged: true")
			}
		case "allowPrivilegeEscalation":
			if v.Value == "true" {
				b.report(rel, entryID, "k8s-privilege-escalation", k.Line, "allowPrivilegeEscalation: true")
			}
		case "hostNetwork", "hostPID", "hostIPC":
			if v.Value == "true" {
				b.report(rel, entryID, "k8s-host-namespace", k.Line, k.Value+": true")
			}
		case "hostPath":
			b.report(rel, entryID, "k8s-host-path", k.Line, "hostPath "+scalar(mapGet(v, "path")))
		case "runAsUser":
			if v.Value == "0" {
				b.report(rel, entryID, "k8s-run-as-root", k.Line, "runAsUser: 0")
			}
		case "runAsNonRoot":
			if v.Value == "false" {
				b.report(rel, entryID, "k8s-run-as-root", k.Line, "runAsNonRoot: false")
			}
		case "add":
			if v.Kind == yaml.SequenceNode {
				for _, c := range v.Content {
					if dangerousCaps[strings.TrimPrefix(c.Value, "CAP_")] {
						b.report(rel, entryID, "k8s-dangerous-capability", c.Line, "capability "+c.Value)
					}
				}
			}
		case "image":
			if v.Kind == yaml.ScalarNode && unpinnedImage(v.Value) {
				b.report(rel, entryID, "k8s-image-unpinned", k.Line, "image "+v.Value)
			}
		}
	})
}

func unpinnedImage(img string) bool {
	if img == "" || templated(img) || strings.Contains(img, "@sha256:") {
		return false
	}
	last := img[strings.LastIndexByte(img, '/')+1:]
	tag := ""
	if i := strings.LastIndexByte(last, ':'); i >= 0 {
		tag = last[i+1:]
	}
	return tag == "" || tag == "latest"
}

var (
	secretKey   = regexp.MustCompile(`(?i)(passw(or)?d|passwd|secret|token|api[_-]?key|apikey|private[_-]?key|access[_-]?key|credential)`)
	placeholder = regexp.MustCompile(`(?i)^(x+|\*+|changeme|change-me.*|example.*|dummy|test|password|secret|token|redacted|todo|none|null|true|false|<.*>)$`)
)

func templated(v string) bool {
	return strings.Contains(v, "{{") || strings.Contains(v, "${") || strings.HasPrefix(v, "$") || strings.Contains(v, tplValue)
}

// genericRules flags literal secrets: key: value pairs and Kubernetes /
// compose env entries ({name: DB_PASSWORD, value: "..."}).
func (b *builder) genericRules(rel, fileID string, docs []*yaml.Node) {
	for _, doc := range docs {
		walk(doc, func(k, v *yaml.Node) {
			if v.Kind != yaml.ScalarNode || v.Tag == "!!bool" || v.Tag == "!!int" {
				return
			}
			if secretKey.MatchString(k.Value) && looksSecret(v.Value) && !isReferenceKey(k.Value, v.Value) {
				b.report(rel, fileID, "yaml-hardcoded-secret", v.Line, fmt.Sprintf("%s holds a %d-character literal", k.Value, len(v.Value)))
			}
			if k.Value == "name" && secretKey.MatchString(v.Value) {
				// Sibling "value" of an env entry is handled by the parent map.
				return
			}
		})
		walk(doc, func(_, v *yaml.Node) {
			if v.Kind != yaml.SequenceNode {
				return
			}
			for _, item := range v.Content {
				name, val := scalar(mapGet(item, "name")), mapGet(item, "value")
				if name != "" && secretKey.MatchString(name) && val != nil && looksSecret(val.Value) {
					b.report(rel, fileID, "yaml-hardcoded-secret", val.Line, fmt.Sprintf("env %s holds a %d-character literal", name, len(val.Value)))
				}
			}
		})
	}
}

var (
	// refSuffix marks keys whose value names or locates a secret.
	refSuffix = regexp.MustCompile(`(?i)(name|ref|file|path|env|id|selector|mount|_key|policy|type|issuer)$`)
	// secretValuedCamel are camelCase keys that do hold secret material.
	secretValuedCamel = map[string]bool{
		"apikey": true, "accesskey": true, "privatekey": true, "secretaccesskey": true, "clientsecret": true,
		"sharedsecret": true, "webhooksecret": true, "password": true, "token": true, "secret": true,
	}
	dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// isReferenceKey recognises the Kubernetes/Helm convention of keys that
// point at a secret rather than contain one: secretName, existingSecret,
// tlsSecret, secretKey (in ExternalSecrets), password_key, and DNS-style
// values under camelCase keys.
func isReferenceKey(key, value string) bool {
	if refSuffix.MatchString(key) {
		return true
	}
	lower := strings.ToLower(key)
	camel := key != lower && key != strings.ToUpper(key) && !strings.Contains(key, "_")
	if camel && !secretValuedCamel[lower] {
		if strings.HasSuffix(lower, "secret") || strings.HasSuffix(lower, "key") || dnsName.MatchString(value) {
			return true
		}
	}
	return false
}

func looksSecret(v string) bool {
	return len(v) >= 8 && !templated(v) && !placeholder.MatchString(v) && !strings.ContainsAny(v, " /") && !isEnvName(v)
}

func isEnvName(v string) bool {
	for _, c := range v {
		if c != '_' && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

var ghaUntrusted = regexp.MustCompile(`\$\{\{\s*github\.(event\.(issue|pull_request|comment|review|review_comment|discussion|pages|commits|head_commit|workflow_run)\b[^}]*|head_ref)\s*\}\}`)

func (b *builder) workflowRules(rel, fileID string, docs []*yaml.Node) {
	for _, doc := range docs {
		root := docRoot(doc)
		prTarget := false
		if on := mapGet(root, "on"); on != nil {
			switch on.Kind {
			case yaml.ScalarNode:
				prTarget = on.Value == "pull_request_target"
			case yaml.SequenceNode:
				for _, c := range on.Content {
					prTarget = prTarget || c.Value == "pull_request_target"
				}
			case yaml.MappingNode:
				prTarget = mapGet(on, "pull_request_target") != nil || hasKey(on, "pull_request_target")
			}
		}
		walk(root, func(k, v *yaml.Node) {
			switch k.Value {
			case "run":
				if m := ghaUntrusted.FindString(v.Value); m != "" {
					b.report(rel, fileID, "gha-script-injection", v.Line, "run step interpolates "+m)
				}
			case "uses":
				action := v.Value
				if strings.HasPrefix(action, "./") || strings.HasPrefix(action, "docker://") {
					return
				}
				owner, ref, _ := strings.Cut(action, "@")
				if !strings.HasPrefix(owner, "actions/") && !strings.HasPrefix(owner, "github/") && !isSHA(ref) {
					b.report(rel, fileID, "gha-unpinned-action", v.Line, "uses "+action)
				}
			case "ref":
				if prTarget && strings.Contains(v.Value, "github.event.pull_request.head") {
					b.report(rel, fileID, "gha-pr-target-checkout", v.Line, "pull_request_target checks out "+v.Value)
				}
			}
		})
	}
}

func hasKey(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return true
		}
	}
	return false
}

func isSHA(ref string) bool {
	if len(ref) != 40 {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func (b *builder) composeRules(rel, fileID string, docs []*yaml.Node) {
	for _, doc := range docs {
		services := mapGet(docRoot(doc), "services")
		if services == nil || services.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(services.Content); i += 2 {
			name, svc := services.Content[i].Value, services.Content[i+1]
			if p := mapGet(svc, "privileged"); p != nil && p.Value == "true" {
				b.report(rel, fileID, "compose-privileged", p.Line, "service "+name+" privileged: true")
			}
			if n := mapGet(svc, "network_mode"); n != nil && n.Value == "host" {
				b.report(rel, fileID, "compose-privileged", n.Line, "service "+name+" network_mode: host")
			}
			if img := mapGet(svc, "image"); img != nil && unpinnedImage(img.Value) {
				b.report(rel, fileID, "k8s-image-unpinned", img.Line, "service "+name+" image "+img.Value)
			}
		}
	}
}
