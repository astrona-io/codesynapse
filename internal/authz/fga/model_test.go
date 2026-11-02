package fga

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	openfga "github.com/openfga/go-sdk"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestModelDSLMatchesJSON renders model.json back into DSL and compares it
// with model.fga relation by relation, so neither file changes alone.
func TestModelDSLMatchesJSON(t *testing.T) {
	m, err := Model()
	require.NoError(t, err)

	fromJSON := map[string]string{}
	for _, td := range m.TypeDefinitions {
		fromJSON[td.Type] = ""
		if td.Relations == nil {
			continue
		}
		for name, us := range *td.Relations {
			var direct []openfga.RelationReference
			if td.Metadata != nil && td.Metadata.Relations != nil {
				if md, ok := (*td.Metadata.Relations)[name]; ok && md.DirectlyRelatedUserTypes != nil {
					direct = *md.DirectlyRelatedUserTypes
				}
			}
			fromJSON[td.Type+"#"+name] = render(t, us, direct)
		}
	}

	fromDSL := map[string]string{}
	typ, schema := "", ""
	for _, line := range strings.Split(modelDSL, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#") || line == "model" || line == "relations":
		case strings.HasPrefix(line, "schema "):
			schema = strings.TrimPrefix(line, "schema ")
		case strings.HasPrefix(line, "type "):
			typ = strings.TrimPrefix(line, "type ")
			fromDSL[typ] = ""
		case strings.HasPrefix(line, "define "):
			name, def, ok := strings.Cut(strings.TrimPrefix(line, "define "), ":")
			require.True(t, ok, "define without colon: %s", line)
			fromDSL[typ+"#"+name] = strings.Join(strings.Fields(def), " ")
		default:
			t.Fatalf("unexpected DSL line %q", line)
		}
	}

	assert.Equal(t, m.SchemaVersion, schema)
	assert.Equal(t, keys(fromDSL), keys(fromJSON))
	for k, want := range fromDSL {
		assert.Equal(t, want, fromJSON[k], "relation %s", k)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// render prints a userset the way model.fga writes it: operands that are
// themselves operations go in parentheses.
func render(t *testing.T, us openfga.Userset, direct []openfga.RelationReference) string {
	t.Helper()
	paren := func(child openfga.Userset) string {
		s := render(t, child, direct)
		if child.Union != nil || child.Intersection != nil || child.Difference != nil {
			return "(" + s + ")"
		}
		return s
	}
	join := func(children []openfga.Userset, op string) string {
		parts := make([]string, len(children))
		for i, c := range children {
			parts[i] = paren(c)
		}
		return strings.Join(parts, " "+op+" ")
	}
	switch {
	case us.This != nil:
		refs := make([]string, len(direct))
		for i, r := range direct {
			refs[i] = r.Type
			if r.Relation != nil {
				refs[i] += "#" + *r.Relation
			}
		}
		return "[" + strings.Join(refs, ", ") + "]"
	case us.ComputedUserset != nil:
		return *us.ComputedUserset.Relation
	case us.TupleToUserset != nil:
		return *us.TupleToUserset.ComputedUserset.Relation + " from " + *us.TupleToUserset.Tupleset.Relation
	case us.Union != nil:
		return join(us.Union.Child, "or")
	case us.Intersection != nil:
		return join(us.Intersection.Child, "and")
	case us.Difference != nil:
		return paren(us.Difference.Base) + " but not " + paren(us.Difference.Subtract)
	}
	t.Fatalf("unknown userset %+v", us)
	return ""
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Compact(&buf, b))
	return buf.Bytes()
}

func TestSameModel(t *testing.T) {
	want, err := Model()
	require.NoError(t, err)
	got := openfga.AuthorizationModel{Id: "x", SchemaVersion: want.SchemaVersion, TypeDefinitions: want.TypeDefinitions}
	assert.True(t, sameModel(want, got))

	// OpenFGA answers with empty fields the request left out.
	answered := strings.NewReplacer(`"relation":`, `"object":"","relation":`, `"type":"user"}`, `"type":"user","condition":""}`).
		Replace(string(compact(t, modelJSON)))
	var stored openfga.AuthorizationModel
	require.NoError(t, json.Unmarshal([]byte(answered), &stored))
	require.True(t, strings.Contains(answered, `"object":""`) && strings.Contains(answered, `"condition":""`))
	assert.True(t, sameModel(want, stored))

	// One relation fewer is a different model.
	other, err := Model()
	require.NoError(t, err)
	for i, td := range other.TypeDefinitions {
		if td.Type == TypeRepository {
			rels := map[string]openfga.Userset{}
			for k, v := range *td.Relations {
				if k != "read" {
					rels[k] = v
				}
			}
			other.TypeDefinitions[i].Relations = &rels
		}
	}
	got.TypeDefinitions = other.TypeDefinitions
	assert.False(t, sameModel(want, got))
}
