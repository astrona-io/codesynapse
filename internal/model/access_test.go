package model

import (
	"fmt"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
)

func TestAccessSnapshotValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*AccessSnapshot)
	}{
		{"tenant", func(s *AccessSnapshot) { s.TenantID = "" }},
		{"revision", func(s *AccessSnapshot) { s.Revision = 0 }},
		{"policy", func(s *AccessSnapshot) { s.Default = "ADMIN" }},
		{"duplicate", func(s *AccessSnapshot) { s.Users = append(s.Users, s.Users[0]) }},
		{"unknown_team", func(s *AccessSnapshot) { s.Members = []AccessMembership{{UserID: "u", TeamID: "missing"}} }},
		{"role", func(s *AccessSnapshot) {
			s.Grants = []AccessGrant{{SubjectKind: KindUser, SubjectID: "u", RepoID: "r", Role: "owner"}}
		}},
		{"duplicate_grants", func(s *AccessSnapshot) {
			s.Grants = []AccessGrant{{SubjectKind: KindUser, SubjectID: "u", RepoID: "r", Role: "READ"}, {SubjectKind: KindUser, SubjectID: "u", RepoID: "r", Role: "ADMIN"}}
		}},
		{"team_limit", func(s *AccessSnapshot) {
			for i := 0; i <= MaxAccessTeams; i++ {
				id := fmt.Sprint(i)
				s.Teams = append(s.Teams, id)
				s.Members = append(s.Members, AccessMembership{UserID: "u", TeamID: id})
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := AccessSnapshot{TenantID: "t", Revision: 1, Default: "NONE", Users: []AccessUser{{ID: "u", Active: true}}, Repos: []AccessRepo{{ID: "r", GraphID: "g", Usable: true}}}
			tc.edit(&s)
			assert.True(t, s.Validate() != nil)
			assert.Equal(t, s.Evaluate("u", "r").Rank, 0)
		})
	}
}
