package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

// normaliseJSON5 rewrites the JSON5 subset Renovate configs use (comments,
// trailing commas, unquoted keys, single-quoted strings) into JSON. Every
// newline of src is kept in place, so line numbers in the output are line
// numbers in the file. Anything it does not understand is copied through
// and left for the JSON decoder to reject.
func normaliseJSON5(src []byte) []byte {
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	out := make([]byte, 0, len(src)+16)
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '"' || c == '\'':
			out, i = copyString(out, src, i)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i < len(src) && (src[i] != '*' || i+1 >= len(src) || src[i+1] != '/') {
				if src[i] == '\n' {
					out = append(out, '\n')
				}
				i++
			}
			i = min(i+2, len(src))
		case c == '}' || c == ']':
			out = append(dropTrailingComma(out), c)
			i++
		case c == '+' || c == '-' || c == '.' || isDigit(c):
			// Copy numbers whole so exponents are not taken for identifiers.
			j := i + 1
			for j < len(src) && (isIdent(src[j]) || src[j] == '.' || src[j] == '+' || src[j] == '-') {
				j++
			}
			if c != '+' {
				out = append(out, src[i:j]...)
			} else {
				out = append(out, src[i+1:j]...)
			}
			i = j
		case isIdent(c):
			j := i + 1
			for j < len(src) && isIdent(src[j]) {
				j++
			}
			switch w := string(src[i:j]); w {
			case "true", "false", "null":
				out = append(out, w...)
			default:
				out = append(append(append(out, '"'), w...), '"')
			}
			i = j
		default:
			out = append(out, c)
			i++
		}
	}
	return out
}

// copyString copies the string literal starting at src[i] as a
// double-quoted JSON string and returns the index after it.
func copyString(out, src []byte, i int) ([]byte, int) {
	q := src[i]
	out = append(out, '"')
	for i++; i < len(src); i++ {
		switch ch := src[i]; {
		case ch == '\\' && i+1 < len(src):
			if src[i+1] == '\'' {
				out = append(out, '\'') // \' is JSON5 only
			} else {
				out = append(out, ch, src[i+1])
			}
			i++
		case ch == q:
			return append(out, '"'), i + 1
		case ch == '"':
			out = append(out, '\\', '"')
		default:
			out = append(out, ch)
		}
	}
	return out, i
}

func dropTrailingComma(out []byte) []byte {
	j := len(out) - 1
	for j >= 0 && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
		j--
	}
	if j >= 0 && out[j] == ',' {
		return append(out[:j], out[j+1:]...)
	}
	return out
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIdent(c byte) bool {
	return c == '_' || c == '$' || isDigit(c) || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

type jsonKind int

const (
	jsonNull jsonKind = iota
	jsonObject
	jsonArray
	jsonString
	jsonNumber
	jsonBool
)

// jsonValue is a decoded JSON value that remembers its line. It lives only
// while rules run; values never reach the graph.
type jsonValue struct {
	kind   jsonKind
	line   int
	str    string // string, or the literal of a number
	b      bool
	fields []jsonField
	items  []*jsonValue
}

type jsonField struct {
	key  string
	line int
	val  *jsonValue
}

// maxJSONDepth stops hostile nesting from exhausting the stack.
const maxJSONDepth = 64

var errJSONDepth = errors.New("nested too deeply")

// jsonError is a parse failure with the line it happened on.
type jsonError struct {
	line int
	err  error
}

func (e *jsonError) Error() string { return fmt.Sprintf("line %d: %v", e.line, e.err) }
func (e *jsonError) Unwrap() error { return e.err }

// parseJSON5 decodes a JSON5 document into a tree with line numbers.
func parseJSON5(src []byte) (*jsonValue, error) {
	data := normaliseJSON5(src)
	var newlines []int
	for i, c := range data {
		if c == '\n' {
			newlines = append(newlines, i)
		}
	}
	lineAt := func(off int64) int { return sort.SearchInts(newlines, int(off)) + 1 }
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	fail := func(err error) error {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return &jsonError{line: lineAt(se.Offset), err: err}
		}
		return &jsonError{line: lineAt(dec.InputOffset()), err: err}
	}
	var decode func(depth int) (*jsonValue, error)
	decode = func(depth int) (*jsonValue, error) {
		if depth > maxJSONDepth {
			return nil, errJSONDepth
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		v := &jsonValue{line: lineAt(max(dec.InputOffset()-1, 0))}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				v.kind = jsonObject
				for dec.More() {
					k, err := dec.Token()
					if err != nil {
						return nil, err
					}
					key, ok := k.(string)
					if !ok {
						return nil, errors.New("object key is not a string")
					}
					line := lineAt(max(dec.InputOffset()-1, 0))
					val, err := decode(depth + 1)
					if err != nil {
						return nil, err
					}
					v.fields = append(v.fields, jsonField{key: key, line: line, val: val})
				}
			case '[':
				v.kind = jsonArray
				for dec.More() {
					item, err := decode(depth + 1)
					if err != nil {
						return nil, err
					}
					v.items = append(v.items, item)
				}
			default:
				return nil, fmt.Errorf("unexpected %q", t)
			}
			if _, err := dec.Token(); err != nil { // closing delimiter
				return nil, err
			}
		case string:
			v.kind, v.str = jsonString, t
		case json.Number:
			v.kind, v.str = jsonNumber, string(t)
		case bool:
			v.kind, v.b = jsonBool, t
		case nil:
			v.kind = jsonNull
		}
		return v, nil
	}
	root, err := decode(0)
	if err != nil {
		return nil, fail(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fail(errors.New("data after the top-level value"))
	}
	return root, nil
}

// field returns the field named key of an object, or nil.
func (v *jsonValue) field(key string) *jsonField {
	if v == nil || v.kind != jsonObject {
		return nil
	}
	for i := range v.fields {
		if v.fields[i].key == key {
			return &v.fields[i]
		}
	}
	return nil
}

// get returns the value of key, or nil.
func (v *jsonValue) get(key string) *jsonValue {
	if f := v.field(key); f != nil {
		return f.val
	}
	return nil
}

func (v *jsonValue) isBool(b bool) bool { return v != nil && v.kind == jsonBool && v.b == b }

// strings returns the string items of an array (or a lone string).
func (v *jsonValue) strings() []string {
	if v == nil {
		return nil
	}
	if v.kind == jsonString {
		return []string{v.str}
	}
	var out []string
	for _, it := range v.items {
		if it.kind == jsonString {
			out = append(out, it.str)
		}
	}
	return out
}
