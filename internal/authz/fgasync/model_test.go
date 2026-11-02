package fgasync

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

var updateModel = flag.Bool("update-model", false, "rewrite internal/authz/fga/model.fga and model.json from the permission table")

// TestModelFiles keeps the OpenFGA model in step with the Go permission
// table: model.fga and model.json are generated here (go test
// ./internal/authz/fgasync -run TestModelFiles -update-model), so a new
// permission or a changed floor cannot leave OpenFGA behind.
func TestModelFiles(t *testing.T) {
	types := modelTypes()
	dsl, js := renderDSL(types), renderJSON(t, types)
	dir := filepath.Join("..", "fga")
	if *updateModel {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "model.fga"), []byte(dsl), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "model.json"), js, 0o644))
	}
	gotDSL, err := os.ReadFile(filepath.Join(dir, "model.fga"))
	require.NoError(t, err)
	gotJSON, err := os.ReadFile(filepath.Join(dir, "model.json"))
	require.NoError(t, err)
	assert.Equal(t, string(gotDSL), dsl, "model.fga is generated; run go test ./internal/authz/fgasync -run TestModelFiles -update-model")
	assert.True(t, bytes.Equal(gotJSON, js), "model.json is generated; run go test ./internal/authz/fgasync -run TestModelFiles -update-model")
}

// TestRelationNames checks every permission has a relation of its own.
func TestRelationNames(t *testing.T) {
	seen := map[string]accounts.Permission{}
	for _, p := range accounts.AllPermissions() {
		rel := Relation(p)
		require.True(t, rel != "", "%s has a relation", p)
		if p.Marker() {
			continue
		}
		prev, dup := seen[p.Resource()+"#"+rel]
		require.False(t, dup, "%s and %s share relation %s", p, prev, rel)
		seen[p.Resource()+"#"+rel] = p
	}
	assert.Equal(t, Relation(accounts.OrgMembership), "active")
	assert.Equal(t, Relation(accounts.OrgOwnerRules), "owner")
	assert.Equal(t, Relation(accounts.RepoReviewsWrite), "reviews_write")
}

// expr is a userset rewrite of the model, rendered both as DSL and JSON.
type expr struct {
	op    string // this, rel, from, or, and, not
	types []string
	rel   string
	from  string
	kids  []expr
}

func direct(types ...string) expr { return expr{op: "this", types: types} }
func rel(r string) expr           { return expr{op: "rel", rel: r} }
func from(r, ts string) expr      { return expr{op: "from", rel: r, from: ts} }
func or(kids ...expr) expr        { return expr{op: "or", kids: kids} }
func and(kids ...expr) expr       { return expr{op: "and", kids: kids} }
func butNot(a, b expr) expr       { return expr{op: "not", kids: []expr{a, b}} }

type relation struct {
	name    string
	comment []string
	def     expr
}

type typeDef struct {
	name      string
	relations []relation
}

// modelTypes is the whole model. The organisation and repository levels
// (read, write, full) are the built-in roles; every permission then gets a
// relation of its own that holds when the permission's floor role does or
// a custom role grants it, so a check asks for the permission it needs.
func modelTypes() []typeDef {
	org := []relation{
		{name: "owner", comment: []string{"A team can hold the owner role too (SetTeamRole), so owner takes", "team#read like the levels below."}, def: direct("user", "team#read")},
		{name: "full", def: or(direct("user", "team#read"), rel("owner"))},
		{name: "write", def: or(direct("user", "team#read"), rel("full"))},
		{name: "read", def: or(direct("user", "team#read"), rel("write"))},
		{name: "guest", def: direct("user")},
		{name: "member", comment: []string{"Active non-guest members; written only for explicit memberships and", "removed when a membership ends or the account is suspended."}, def: direct("user")},
		{name: "active", def: or(rel("member"), rel("guest"))},
		{name: "default_readers", comment: []string{"Written as organisation:O#default_readers@organisation:O#member only", "while the organisation's default is READ_ONLY."}, def: direct("organisation#member")},
	}
	repo := []relation{
		{name: "organisation", def: direct("organisation")},
		{name: "org_space", comment: []string{"Set only for organisation-space entries, never personal ones."}, def: direct("organisation")},
		{name: "direct_full", def: direct("user")},
		{name: "direct_write", def: direct("user")},
		{name: "direct_read", def: direct("user")},
		{name: "direct_deny", def: direct("user")},
		{name: "direct_custom", comment: []string{"A user's custom role on the entry; its permissions are the user_*", "relations below. Written also when the role is gone, so it still", "replaces team grants and the default while granting nothing."}, def: direct("user")},
		{name: "has_direct", def: or(rel("direct_full"), rel("direct_write"), rel("direct_read"), rel("direct_deny"), rel("direct_custom"))},
		{name: "team_full", def: direct("team#read")},
		{name: "team_write", def: direct("team#read")},
		{name: "team_read", def: direct("team#read")},
		{name: "team_custom", comment: []string{"A team's custom role on the entry, like direct_custom: its", "permissions are the team_* relations below."}, def: direct("team#read")},
		{name: "has_team", def: or(rel("team_full"), rel("team_write"), rel("team_read"), rel("team_custom"))},
		{name: "default_read", def: from("default_readers", "org_space")},
		{name: "full", comment: []string{"A direct grant replaces team grants and the default, even when weaker;", "a team grant replaces the default."},
			def: and(or(rel("direct_full"), butNot(rel("team_full"), rel("has_direct"))), from("active", "organisation"))},
		{name: "write", def: and(or(rel("full"), rel("direct_write"), butNot(rel("team_write"), rel("has_direct"))), from("active", "organisation"))},
		{name: "read", def: and(or(rel("write"), rel("direct_read"), butNot(or(rel("team_read"), butNot(rel("default_read"), rel("has_team"))), rel("has_direct"))), from("active", "organisation"))},
	}
	repoFirst, orgFirst := true, true
	for _, p := range accounts.AllPermissions() {
		if p.Marker() {
			continue
		}
		name, floor, higher := Relation(p), floorRelation(p), higherRelation(p)
		switch p.Resource() {
		case accounts.ResourceRepository:
			user, team := "user_"+name, "team_"+name
			var c []string
			if repoFirst {
				c = []string{"One relation per permission (repository.reviews:write is", "reviews_write): its floor role, the level above it in the same area,", "or a custom role's grant to the user or, without a direct grant, to", "one of the user's teams."}
				repoFirst = false
			}
			kids := []expr{rel(floor)}
			if higher != "" {
				kids = append(kids, rel(higher))
			}
			kids = append(kids, rel(user), butNot(rel(team), rel("has_direct")))
			repo = append(repo,
				relation{name: user, comment: c, def: direct("user")},
				relation{name: team, def: direct("team#read")},
				relation{name: name, def: and(or(kids...), from("active", "organisation"))})
		case accounts.ResourceOrganisation:
			var c []string
			if orgFirst {
				c = []string{"One relation per permission (organisation.members:read is", "members_read): its floor role, the level above it in the same area,", "or a custom role granted to the member or one of their teams."}
				orgFirst = false
			}
			kids := []expr{direct("user", "team#read"), rel(floor)}
			if higher != "" {
				kids = append(kids, rel(higher))
			}
			org = append(org, relation{name: name, comment: c, def: and(or(kids...), rel("active"))})
		}
	}
	return []typeDef{
		{name: fga.TypeUser},
		{name: fga.TypeOrganisation, relations: org},
		{name: fga.TypeTeam, relations: []relation{
			{name: "organisation", def: direct("organisation")},
			{name: "full", def: direct("user")},
			{name: "read", def: or(direct("user"), rel("full"))},
		}},
		{name: fga.TypeRepository, relations: repo},
	}
}

// floorRelation is the level relation of p's floor role; a guest floor is
// every active member.
func floorRelation(p accounts.Permission) string {
	if p.Floor() == accounts.Guest {
		return "active"
	}
	return orgLevel[p.Floor()]
}

// higherRelation is the next level of p's area ("" at the top).
func higherRelation(p accounts.Permission) string {
	order := []string{accounts.LevelRead, accounts.LevelWrite, accounts.LevelFull}
	at := -1
	for i, l := range order {
		if l == p.Level() {
			at = i
		}
	}
	for _, l := range order[at+1:] {
		q := accounts.Permission(p.Resource() + "." + p.Area() + ":" + l)
		if q.Valid() {
			return Relation(q)
		}
	}
	return ""
}

const modelHeader = `# Authorisation model for repository and organisation access
# (docs/plans/OPENFGA_PLAN.md). Generated from the Go permission table by
# TestModelFiles in internal/authz/fgasync; do not edit by hand. model.json
# is the same model in OpenFGA's JSON form, which the client writes.
model
  schema 1.1
`

func renderDSL(types []typeDef) string {
	var b strings.Builder
	b.WriteString(modelHeader)
	for _, td := range types {
		b.WriteString("\ntype " + td.name + "\n")
		if len(td.relations) == 0 {
			continue
		}
		b.WriteString("  relations\n")
		for _, r := range td.relations {
			for _, c := range r.comment {
				b.WriteString("    # " + c + "\n")
			}
			b.WriteString("    define " + r.name + ": " + dslOf(r.def) + "\n")
		}
	}
	return b.String()
}

func dslOf(e expr) string {
	paren := func(k expr) string {
		if k.op == "or" || k.op == "and" || k.op == "not" {
			return "(" + dslOf(k) + ")"
		}
		return dslOf(k)
	}
	join := func(sep string) string {
		parts := make([]string, len(e.kids))
		for i, k := range e.kids {
			parts[i] = paren(k)
		}
		return strings.Join(parts, " "+sep+" ")
	}
	switch e.op {
	case "this":
		return "[" + strings.Join(e.types, ", ") + "]"
	case "rel":
		return e.rel
	case "from":
		return e.rel + " from " + e.from
	case "or":
		return join("or")
	case "and":
		return join("and")
	default:
		return paren(e.kids[0]) + " but not " + paren(e.kids[1])
	}
}

func jsonOf(e expr) map[string]any {
	kids := func() []any {
		out := make([]any, len(e.kids))
		for i, k := range e.kids {
			out[i] = jsonOf(k)
		}
		return out
	}
	switch e.op {
	case "this":
		return map[string]any{"this": map[string]any{}}
	case "rel":
		return map[string]any{"computedUserset": map[string]any{"relation": e.rel}}
	case "from":
		return map[string]any{"tupleToUserset": map[string]any{
			"computedUserset": map[string]any{"relation": e.rel},
			"tupleset":        map[string]any{"relation": e.from},
		}}
	case "or":
		return map[string]any{"union": map[string]any{"child": kids()}}
	case "and":
		return map[string]any{"intersection": map[string]any{"child": kids()}}
	default:
		return map[string]any{"difference": map[string]any{"base": jsonOf(e.kids[0]), "subtract": jsonOf(e.kids[1])}}
	}
}

// directTypes is the type restriction of e's direct grant, if it has one.
func directTypes(e expr) []string {
	if e.op == "this" {
		return e.types
	}
	for _, k := range e.kids {
		if ts := directTypes(k); ts != nil {
			return ts
		}
	}
	return nil
}

func renderJSON(t *testing.T, types []typeDef) []byte {
	t.Helper()
	defs := make([]any, 0, len(types))
	for _, td := range types {
		if len(td.relations) == 0 {
			defs = append(defs, map[string]any{"type": td.name})
			continue
		}
		rels, meta := map[string]any{}, map[string]any{}
		for _, r := range td.relations {
			rels[r.name] = jsonOf(r.def)
			md := map[string]any{}
			if ts := directTypes(r.def); ts != nil {
				refs := make([]any, len(ts))
				for i, s := range ts {
					typ, relName, ok := strings.Cut(s, "#")
					ref := map[string]any{"type": typ}
					if ok {
						ref["relation"] = relName
					}
					refs[i] = ref
				}
				md["directly_related_user_types"] = refs
			}
			meta[r.name] = md
		}
		defs = append(defs, map[string]any{"type": td.name, "relations": rels, "metadata": map[string]any{"relations": meta}})
	}
	out, err := json.MarshalIndent(map[string]any{"schema_version": "1.1", "type_definitions": defs}, "", "  ")
	require.NoError(t, err)
	return append(out, '\n')
}
