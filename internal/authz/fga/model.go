// Package fga holds the OpenFGA authorisation model for repository and
// organisation access and a client that fails closed
// (docs/plans/OPENFGA_PLAN.md, phase 4). Nothing asks it for decisions yet:
// the outbox sync is phase 5 and the cutover phase 6.
package fga

import (
	"bytes"
	"encoding/json"
	"fmt"

	_ "embed"

	openfga "github.com/openfga/go-sdk"
)

// model.json is what OpenFGA's own transformer (`fga model transform`)
// makes of model.fga. The JSON is embedded rather than transformed at run
// time so the binary needs no DSL parser; TestModelDSLMatchesJSON keeps the
// two files in step.
//
//go:embed model.json
var modelJSON []byte

//go:embed model.fga
var modelDSL string

// Object types of the model.
const (
	TypeUser         = "user"
	TypeOrganisation = "organisation"
	TypeTeam         = "team"
	TypeRepository   = "repository"
)

// Model returns the authorisation model as OpenFGA's write request.
func Model() (openfga.WriteAuthorizationModelRequest, error) {
	var m openfga.WriteAuthorizationModelRequest
	dec := json.NewDecoder(bytes.NewReader(modelJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("decode authorisation model: %w", err)
	}
	if m.SchemaVersion != "1.1" || len(m.TypeDefinitions) == 0 {
		return m, fmt.Errorf("authorisation model: schema %q with %d types", m.SchemaVersion, len(m.TypeDefinitions))
	}
	return m, nil
}

// sameModel reports whether a stored model has the same schema, types and
// relations as want. Relation maps compare as sets, so the order OpenFGA
// returns them in does not matter.
func sameModel(want openfga.WriteAuthorizationModelRequest, got openfga.AuthorizationModel) bool {
	if want.SchemaVersion != got.SchemaVersion || len(want.TypeDefinitions) != len(got.TypeDefinitions) {
		return false
	}
	a, errA := canonicalTypes(want.TypeDefinitions)
	b, errB := canonicalTypes(got.TypeDefinitions)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// canonicalTypes encodes type definitions by name with empty relation and
// metadata maps dropped: OpenFGA may answer {} where the request had none.
// encoding/json sorts map keys, which makes the result comparable.
func canonicalTypes(defs []openfga.TypeDefinition) ([]byte, error) {
	type rel struct {
		Def   openfga.Userset              `json:"def"`
		Types *[]openfga.RelationReference `json:"types,omitempty"`
	}
	out := map[string]map[string]rel{}
	for _, d := range defs {
		rels := map[string]rel{}
		if d.Relations != nil {
			for name, us := range *d.Relations {
				r := rel{Def: us}
				if d.Metadata != nil && d.Metadata.Relations != nil {
					if md, ok := (*d.Metadata.Relations)[name]; ok && md.DirectlyRelatedUserTypes != nil && len(*md.DirectlyRelatedUserTypes) > 0 {
						r.Types = md.DirectlyRelatedUserTypes
					}
				}
				rels[name] = r
			}
		}
		if _, dup := out[d.Type]; dup {
			return nil, fmt.Errorf("type %q defined twice", d.Type)
		}
		out[d.Type] = rels
	}
	// OpenFGA answers "object": "" and "condition": "" where the request had
	// no field; encode, then drop every empty value before comparing.
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	return json.Marshal(dropEmpty(tree))
}

// dropEmpty removes map entries that are empty strings, nulls or empty
// arrays. Empty maps stay: "this": {} is how OpenFGA writes a direct grant.
func dropEmpty(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			switch c := child.(type) {
			case nil:
				delete(t, k)
			case string:
				if c == "" {
					delete(t, k)
				}
			case []any:
				if len(c) == 0 {
					delete(t, k)
				} else {
					t[k] = dropEmpty(c)
				}
			default:
				t[k] = dropEmpty(c)
			}
		}
		return t
	case []any:
		for i, c := range t {
			t[i] = dropEmpty(c)
		}
		return t
	}
	return v
}
