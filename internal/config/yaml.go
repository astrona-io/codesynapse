package config

import "gopkg.in/yaml.v3"

var fields = map[string]map[string]string{
	"":                                 {"version": "int", "ignore": "map", "scanners": "map", "annotation": "map"},
	"ignore":                           {"paths": "strings"},
	"scanners":                         {"kubernetes": "map", "security": "map"},
	"scanners.kubernetes":              {"enabled": "bool", "ignore_paths": "strings"},
	"scanners.security":                {"ignore_rules": "rules", "inline_ignores": "bool"},
	"scanners.security.ignore_rules[]": {"rule_id": "string", "paths": "strings"},
	"annotation":                       {"enabled": "bool", "provider_task": "string", "max_batch_size": "int", "ignore_paths": "strings", "taxonomy_hints": "strings"},
}

func checkNode(n *yaml.Node, field string, depth int, count *int) error {
	*count++
	if depth > 16 || *count > 4096 {
		return invalid(field, n.Line, "document complexity limit exceeded")
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return invalid(field, n.Line, "aliases and anchors are not allowed")
	}
	switch n.Tag {
	case "!!map", "!!seq", "!!str", "!!int", "!!bool":
	default:
		return invalid(field, n.Line, "unsupported value or tag")
	}
	if n.Kind != yaml.MappingNode {
		return invalid(field, n.Line, "expected a mapping")
	}
	seen := make(map[string]bool)
	for i := 0; i < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Anchor != "" {
			return invalid(field, key.Line, "expected a plain field name")
		}
		expected, ok := fields[field][key.Value]
		if !ok {
			return invalid(field, key.Line, "unknown field")
		}
		child := key.Value
		if field != "" {
			child = field + "." + child
		}
		if seen[key.Value] {
			return invalid(child, key.Line, "duplicate field")
		}
		seen[key.Value] = true
		*count++
		if err := checkValue(val, child, expected, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}
func checkValue(n *yaml.Node, field, expected string, depth int, count *int) error {
	*count++
	if depth > 16 || *count > 4096 {
		return invalid(field, n.Line, "document complexity limit exceeded")
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return invalid(field, n.Line, "aliases and anchors are not allowed")
	}
	switch expected {
	case "map":
		return checkNode(n, field, depth, count)
	case "strings", "rules":
		if n.Kind != yaml.SequenceNode || n.Tag != "!!seq" {
			return invalid(field, n.Line, "expected a sequence")
		}
		itemType := "string"
		if expected == "rules" {
			itemType = "map"
		}
		for _, child := range n.Content {
			if err := checkValue(child, field+"[]", itemType, depth+1, count); err != nil {
				return err
			}
		}
	default:
		tag := map[string]string{"string": "!!str", "int": "!!int", "bool": "!!bool"}[expected]
		if n.Kind != yaml.ScalarNode || n.Tag != tag {
			return invalid(field, n.Line, "invalid field type")
		}
	}
	return nil
}
func fieldLine(doc *yaml.Node, field string) int {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return fieldLine(doc.Content[0], field)
	}
	if doc.Kind != yaml.MappingNode {
		return 0
	}
	for i := 0; i < len(doc.Content); i += 2 {
		key, val := doc.Content[i], doc.Content[i+1]
		if key.Value == field {
			return val.Line
		}
		if len(field) > len(key.Value) && field[:len(key.Value)] == key.Value && field[len(key.Value)] == '.' {
			return fieldLine(val, field[len(key.Value)+1:])
		}
	}
	return 0
}
