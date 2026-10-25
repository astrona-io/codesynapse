package config

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/redact"
)

// Terraform (security roadmap item 9): .tf files are read with hcl.go and
// checked for network rules open to the internet, public storage,
// unencrypted storage, publicly reachable databases and literal secrets,
// on AWS, Google Cloud and Azure resources. Only what the file states
// literally counts: a value from a variable or module is not guessed, and
// a secret finding names the attribute, never the value.

// tfOpen are the address ranges that mean "anyone".
var tfOpen = []string{"0.0.0.0/0", "::/0", "*", "Internet", "Any"}

func (b *builder) terraformFile(rel string, content []byte) {
	lines := bytes.Count(content, []byte("\n")) + 1
	fileID := b.id(model.KindConfig, rel)
	blocks := parseHCL(string(content))
	types := map[string]int{}
	for _, blk := range blocks {
		if blk.Type == "resource" && len(blk.Labels) > 0 {
			types[blk.Labels[0]]++
		}
	}
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	summary := fmt.Sprintf("Terraform: %d block(s)", len(blocks))
	if len(names) > 0 {
		summary += ", resources " + strings.Join(names[:min(len(names), 6)], ", ")
	}
	b.g.AddNode(model.Node{
		ID: fileID, Kind: model.KindConfig, Repo: b.repo, Name: path.Base(rel), File: rel, Language: "terraform",
		StartLine: 1, EndLine: lines, LOC: lines, Bytes: len(content), Summary: redact.Summary(summary, 200),
	})
	b.g.AddEdge(model.Edge{From: model.NodeID(b.repo, model.KindRepository, b.repo), To: fileID, Kind: model.EdgeContains})

	report := func(rule string, line int, detail string) {
		b.reportIn(rel, fileID, rule, line, detail, "terraform")
	}
	for _, blk := range blocks {
		switch blk.Type {
		case "resource":
			if len(blk.Labels) < 2 {
				continue
			}
			b.terraformResource(blk, blk.Labels[0], blk.Labels[0]+"."+blk.Labels[1], report)
		case "variable":
			// A default for a secret variable is a secret in the repository.
			if a, ok := blk.Attrs["default"]; ok && len(blk.Labels) > 0 && secretKey.MatchString(blk.Labels[0]) && a.Literal && looksSecret(unescapeHCL(a.Strings[0])) {
				report("tf-secret-literal", a.Line, fmt.Sprintf("variable %s has a literal default", redact.Summary(blk.Labels[0], 60)))
			}
		case "provider":
			b.terraformSecrets(blk, "provider "+strings.Join(blk.Labels, " "), report)
		}
	}
}

func (b *builder) terraformResource(blk *hclBlock, typ, name string, report func(string, int, string)) {
	attr := func(k string) (hclAttr, bool) { a, ok := blk.Attrs[k]; return a, ok }
	str := func(k string) string {
		if a, ok := attr(k); ok && a.Literal {
			return unescapeHCL(a.Strings[0])
		}
		return ""
	}
	isFalse := func(k string) (hclAttr, bool) { a, ok := attr(k); return a, ok && a.Bool == "false" }
	isTrue := func(k string) (hclAttr, bool) { a, ok := attr(k); return a, ok && a.Bool == "true" }

	switch typ {
	case "aws_security_group":
		for _, rule := range blk.Blocks {
			if rule.Type == "ingress" {
				b.tfOpenIngress(rule, name, report)
			}
		}
	case "aws_security_group_rule":
		if str("type") == "ingress" {
			b.tfOpenIngress(blk, name, report)
		}
	case "aws_vpc_security_group_ingress_rule":
		b.tfOpenIngress(blk, name, report)
	case "google_compute_firewall":
		if d := str("direction"); d == "" || strings.EqualFold(d, "INGRESS") {
			if a, ok := attr("source_ranges"); ok && anyOpen(a.Strings) {
				var ports []string
				for _, allow := range blk.Blocks {
					if allow.Type == "allow" {
						if p, ok := allow.Attrs["ports"]; ok {
							ports = append(ports, p.Strings...)
						} else {
							ports = append(ports, "all")
						}
					}
				}
				if !onlyWebPorts(ports) {
					report("tf-open-ingress", a.Line, name+" allows traffic from anywhere")
				}
			}
		}
	case "azurerm_network_security_rule":
		if strings.EqualFold(str("direction"), "Inbound") && strings.EqualFold(str("access"), "Allow") && anyOpen([]string{str("source_address_prefix")}) &&
			!onlyWebPorts([]string{str("destination_port_range")}) {
			a, _ := attr("source_address_prefix")
			report("tf-open-ingress", a.Line, name+" allows inbound traffic from anywhere")
		}

	case "aws_s3_bucket", "aws_s3_bucket_acl":
		if acl := str("acl"); strings.HasPrefix(acl, "public-") {
			a, _ := attr("acl")
			report("tf-public-storage", a.Line, fmt.Sprintf("%s has the %s ACL", name, acl))
		}
	case "aws_s3_bucket_public_access_block", "aws_s3_account_public_access_block":
		for _, k := range []string{"block_public_acls", "block_public_policy", "ignore_public_acls", "restrict_public_buckets"} {
			if a, off := isFalse(k); off {
				report("tf-public-storage", a.Line, fmt.Sprintf("%s turns %s off", name, k))
				break
			}
		}
	case "google_storage_bucket_iam_member", "google_storage_bucket_iam_binding":
		for _, k := range []string{"member", "members"} {
			if a, ok := attr(k); ok && (slices.Contains(a.Strings, "allUsers") || slices.Contains(a.Strings, "allAuthenticatedUsers")) {
				report("tf-public-storage", a.Line, name+" grants access to all users")
			}
		}
	case "azurerm_storage_container":
		if t := str("container_access_type"); t == "blob" || t == "container" {
			a, _ := attr("container_access_type")
			report("tf-public-storage", a.Line, fmt.Sprintf("%s allows anonymous %s access", name, t))
		}
	case "azurerm_storage_account":
		if a, on := isTrue("allow_nested_items_to_be_public"); on {
			report("tf-public-storage", a.Line, name+" allows public containers")
		}

	case "aws_db_instance", "aws_rds_cluster":
		if a, on := isTrue("publicly_accessible"); on {
			report("tf-public-database", a.Line, name+" is reachable from the internet")
		}
		if a, off := isFalse("storage_encrypted"); off {
			report("tf-unencrypted-storage", a.Line, name+" turns storage encryption off")
		} else if _, set := attr("storage_encrypted"); !set && typ == "aws_db_instance" && !strings.HasPrefix(str("engine"), "aurora") {
			report("tf-unencrypted-storage", blk.Line, name+" does not enable storage_encrypted (off by default)")
		}
	case "aws_ebs_volume":
		if _, set := attr("encrypted"); !set {
			report("tf-unencrypted-storage", blk.Line, name+" does not enable encrypted (off by default unless the account encrypts by default)")
		} else if a, off := isFalse("encrypted"); off {
			report("tf-unencrypted-storage", a.Line, name+" turns encryption off")
		}
	case "google_sql_database_instance":
		for _, s := range blk.Blocks {
			if s.Type != "settings" {
				continue
			}
			for _, ip := range s.Blocks {
				if ip.Type != "ip_configuration" {
					continue
				}
				for _, net := range ip.Blocks {
					if v, ok := net.Attrs["value"]; ok && net.Type == "authorized_networks" && anyOpen(v.Strings) {
						report("tf-public-database", v.Line, name+" authorizes connections from anywhere")
					}
				}
			}
		}
	}
	b.terraformSecrets(blk, name, report)
}

// tfOpenIngress reports an ingress rule open to any address on ports other
// than HTTP and HTTPS, which public web servers are meant to expose.
func (b *builder) tfOpenIngress(rule *hclBlock, name string, report func(string, int, string)) {
	for _, k := range []string{"cidr_blocks", "ipv6_cidr_blocks", "cidr_ipv4", "cidr_ipv6"} {
		a, ok := rule.Attrs[k]
		if !ok || !anyOpen(a.Strings) {
			continue
		}
		from, to := literalPort(rule.Attrs["from_port"]), literalPort(rule.Attrs["to_port"])
		if from != "" && from == to && (from == "80" || from == "443") {
			return
		}
		ports := "all ports"
		if from != "" {
			ports = "port " + from
			if to != "" && to != from {
				ports = "ports " + from + "-" + to
			}
		}
		report("tf-open-ingress", a.Line, fmt.Sprintf("%s allows %s from anywhere", name, ports))
		return
	}
}

// terraformSecrets reports secret-named attributes with a literal value.
func (b *builder) terraformSecrets(blk *hclBlock, name string, report func(string, int, string)) {
	keys := make([]string, 0, len(blk.Attrs))
	for k := range blk.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := blk.Attrs[k]
		if secretKey.MatchString(k) && !strings.HasSuffix(k, "_arn") && !strings.HasSuffix(k, "_id") && a.Literal && looksSecret(unescapeHCL(a.Strings[0])) {
			report("tf-secret-literal", a.Line, fmt.Sprintf("%s sets %s to a literal value", name, k))
		}
	}
}

func anyOpen(values []string) bool {
	for _, v := range values {
		if slices.ContainsFunc(tfOpen, func(o string) bool { return strings.EqualFold(o, strings.TrimSpace(v)) }) {
			return true
		}
	}
	return false
}

func onlyWebPorts(ports []string) bool {
	if len(ports) == 0 {
		return false
	}
	for _, p := range ports {
		if p != "80" && p != "443" {
			return false
		}
	}
	return true
}

func literalPort(a hclAttr) string {
	if a.Raw != "" && !strings.ContainsAny(a.Raw, ".\"(") {
		return a.Raw
	}
	return ""
}

// unescapeHCL undoes the escapes a literal string can hold.
func unescapeHCL(s string) string {
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t").Replace(s)
}
