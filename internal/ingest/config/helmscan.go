package config

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// tplValue stands in for a template action inside a value or key. The rules
// treat it like any templated value: unknown, so never flagged.
const tplValue = "__TPL__"

// neutralize turns a Helm template into YAML the security rules can read
// without rendering it: lines holding only template actions become blank,
// actions that emit a block (nindent, indent, toYaml) become nothing, and
// other actions inside text become tplValue. Line numbers are kept, so
// findings point at the template's own lines. What a template only produces
// through {{include}} or toYaml is not seen; literal settings are.
func neutralize(src []byte) []byte {
	spans := actionSpans(src)
	var out bytes.Buffer
	pos := 0
	for lineNo, line := range bytes.Split(src, []byte("\n")) {
		if lineNo > 0 {
			out.WriteByte('\n')
		}
		start, end := pos, pos+len(line)
		pos = end + 1
		var text bytes.Buffer
		onlyActions := true
		at := start
		for _, sp := range spans {
			if sp[1] <= start || sp[0] >= end {
				continue
			}
			from, to := max(sp[0], start), min(sp[1], end)
			if from > at {
				seg := src[at:from]
				text.Write(seg)
				if len(bytes.TrimSpace(seg)) > 0 {
					onlyActions = false
				}
			}
			// The placeholder goes where the action starts; a block-emitting
			// action leaves the key empty so its indented children parse.
			if sp[0] >= start && !emitsBlock(src[sp[0]:sp[1]]) {
				text.WriteString(tplValue)
			}
			at = to
		}
		if at < end {
			seg := src[at:end]
			text.Write(seg)
			if len(bytes.TrimSpace(seg)) > 0 {
				onlyActions = false
			}
		}
		if onlyActions {
			continue // blank line: the action produced structure we cannot see
		}
		out.Write(bytes.TrimRight(text.Bytes(), " \t"))
	}
	return out.Bytes()
}

// emitsBlock reports actions whose output is indented YAML, not a value.
func emitsBlock(action []byte) bool {
	return bytes.Contains(action, []byte("nindent")) || bytes.Contains(action, []byte("toYaml")) ||
		bytes.Contains(action, []byte("| indent")) || bytes.Contains(action, []byte("|indent"))
}

// actionSpans returns the [start, end) byte ranges of every {{ ... }}
// action, skipping strings, raw strings and comments inside actions so a
// "}}" in a quoted string does not end the action.
func actionSpans(src []byte) [][2]int {
	var spans [][2]int
	for i := 0; i+1 < len(src); {
		if src[i] != '{' || src[i+1] != '{' {
			i++
			continue
		}
		start := i
		i += 2
		closed := false
		for i < len(src) && !closed {
			switch {
			case bytes.HasPrefix(src[i:], []byte("/*")):
				if k := bytes.Index(src[i+2:], []byte("*/")); k >= 0 {
					i += k + 4
				} else {
					i = len(src)
				}
			case src[i] == '"':
				i++
				for i < len(src) && src[i] != '"' && src[i] != '\n' {
					if src[i] == '\\' {
						i++
					}
					i++
				}
				i++
			case src[i] == '`':
				if k := bytes.IndexByte(src[i+1:], '`'); k >= 0 {
					i += k + 2
				} else {
					i = len(src)
				}
			case src[i] == '}' && i+1 < len(src) && src[i+1] == '}':
				i += 2
				closed = true
			default:
				i++
			}
		}
		spans = append(spans, [2]int{start, min(i, len(src))})
	}
	return spans
}

// scanTemplate runs the YAML security rules over a neutralised Helm
// template. It reports whether the template could be read as YAML.
func (b *builder) scanTemplate(rel, ownerID string, content []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(neutralize(content)))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false
		}
		docs = append(docs, &doc)
	}
	for _, doc := range docs {
		if root := docRoot(doc); root != nil && root.Kind == yaml.MappingNode {
			if _, ok := k8sResource(root); ok || strings.TrimSpace(scalar(mapGet(root, "kind"))) == tplValue {
				b.k8sRules(rel, ownerID, root)
			}
		}
	}
	b.genericRules(rel, ownerID, docs)
	return true
}
