package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestNormaliseJSON5(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"line comment", "{\"a\": 1 // note\n}", "{\"a\": 1 \n}"},
		{"block comment keeps newlines", "{/* a\nb */\"a\": 1}", "{\n\"a\": 1}"},
		{"trailing commas", `{"a": [1, 2,], }`, `{"a": [1, 2] }`},
		{"unquoted keys", `{extends: ["x"], $schema: "s"}`, `{"extends": ["x"], "$schema": "s"}`},
		{"single quotes", `{'a': 'it\'s "x"'}`, `{"a": "it's \"x\""}`},
		{"literals and numbers", `{a: true, b: null, c: -1.5e3, d: +2}`, `{"a": true, "b": null, "c": -1.5e3, "d": 2}`},
		{"comment markers inside strings", `{"u": "https://x//y/*z*/"}`, `{"u": "https://x//y/*z*/"}`},
		{"bom", "\xef\xbb\xbf{}", "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(normaliseJSON5([]byte(tt.in)))
			assert.Equal(t, got, tt.want)
			assert.True(t, json.Valid([]byte(got)), got)
		})
	}
}

func TestParseJSON5Lines(t *testing.T) {
	src := "{\n  // comment\n  extends: ['config:recommended'],\n  packageRules: [\n    {automerge: true,},\n  ],\n}\n"
	v, err := parseJSON5([]byte(src))
	require.NoError(t, err)
	assert.Equal(t, v.field("extends").line, 3)
	rules := v.get("packageRules")
	require.Len(t, rules.items, 1)
	assert.Equal(t, rules.items[0].field("automerge").line, 5)
	assert.True(t, rules.items[0].get("automerge").isBool(true))

	_, err = parseJSON5([]byte("{\n  \"a\": 1\n  \"b\": 2\n}"))
	var je *jsonError
	require.True(t, errors.As(err, &je), err)
	assert.Equal(t, je.line, 3)

	_, err = parseJSON5([]byte("{} {}"))
	assert.Error(t, err)
	_, err = parseJSON5(bytes.Repeat([]byte("["), maxJSONDepth+2))
	assert.Error(t, err)
}

func FuzzNormaliseJSON5(f *testing.F) {
	for _, s := range []string{
		`{"a": [1, 2, {"b": null}], "c": "x\"y"}`,
		"{a: 'b', /* c\n */ d: [1,],}\n// e",
		`{"extends": ["config:best-practices"], "packageRules": [{"automerge": true}]}`,
		`'unterminated`, `/*`, `[-]`, `{"a":1e5}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out := normaliseJSON5(in)
		trimmed := bytes.TrimPrefix(in, []byte("\xef\xbb\xbf"))
		if bytes.Count(out, []byte("\n")) != bytes.Count(trimmed, []byte("\n")) {
			t.Fatalf("newlines not preserved: %q -> %q", in, out)
		}
		// Valid JSON is already valid JSON5 and must mean the same thing.
		var want any
		if json.Unmarshal(trimmed, &want) == nil {
			var got any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("valid JSON %q became invalid %q: %v", in, out, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("value changed: %q -> %q", in, out)
			}
		}
		_, _ = parseJSON5(in)
	})
}
