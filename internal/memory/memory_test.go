package memory

import (
	"strings"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestPrepare(t *testing.T) {
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	b, err := Prepare("r", []Triple{
		{Subject: " Project  Alpha ", SubjectType: "Project", Predicate: "uses framework", Object: "FastAPI"},
		{Subject: "project alpha", Predicate: "uses-framework", Object: "fastapi"}, // same fact
		{Subject: "User", Predicate: "PREFERS", Object: "Dark mode", Replace: true},
	}, "", now)
	require.NoError(t, err)
	assert.Equal(t, len(b.Entities), 4)
	assert.Equal(t, b.Entities[0], Entity{ID: "r|mem|project alpha", Repo: "r", Name: "Project Alpha", Type: "Project", CreatedAt: "2026-10-22T08:00:00Z", UpdatedAt: "2026-10-22T08:00:00Z"})
	assert.Equal(t, b.Entities[1].Type, "Thing")
	require.Len(t, b.Facts, 2, "duplicates in one call collapse")
	assert.Equal(t, b.Facts[0].Predicate, "USES_FRAMEWORK")
	assert.Equal(t, b.Facts[0].Source, "agent")
	require.Len(t, b.Replace, 1)
	assert.Equal(t, b.Replace[0].Predicate, "PREFERS")

	b, err = Prepare("r", []Triple{{Subject: "Support line", Predicate: "REACHED_AT", Object: "+45 28 12 34 56"}}, "", now)
	require.NoError(t, err)
	assert.Equal(t, b.Entities[1].Name, "[PHONE]", "personal data is masked in names")
}

func TestPrepareRejects(t *testing.T) {
	ok := Triple{Subject: "a", Predicate: "P", Object: "b"}
	many := make([]Triple, MaxTriplesPerCall+1)
	for i := range many {
		many[i] = ok
	}
	tests := []struct {
		name    string
		repo    string
		triples []Triple
		want    string
	}{
		{"no repo", "", []Triple{ok}, "repo is required"},
		{"none", "r", nil, "give 1 to"},
		{"too many", "r", many, "give 1 to"},
		{"bad predicate", "r", []Triple{{Subject: "a", Predicate: "drop (n) //", Object: "b"}}, "predicate"},
		{"empty subject", "r", []Triple{{Subject: "  ", Predicate: "P", Object: "b"}}, "non-empty"},
		{"bad type", "r", []Triple{{Subject: "a", SubjectType: "x y", Predicate: "P", Object: "b"}}, "type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Prepare(tt.repo, tt.triples, "a", time.Now())
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), tt.want), err.Error())
		})
	}
}

func TestPrepareRedactsAndTruncates(t *testing.T) {
	b, err := Prepare("r", []Triple{{
		Subject: "deploy\x00key", Predicate: "HAS",
		Object: "token=ghp_abcdefghijklmnopqrstuvwxyz0123456789 " + strings.Repeat("x", 300),
	}}, "claude-code", time.Now())
	require.NoError(t, err)
	assert.Equal(t, b.Entities[0].Name, "deploy key", "control characters become spaces")
	obj := b.Entities[1].Name
	assert.False(t, strings.Contains(obj, "ghp_abc"), obj)
	assert.True(t, len([]rune(obj)) <= MaxNameLen, "truncated")
}

func TestWordsAndScore(t *testing.T) {
	assert.Equal(t, Words("FastAPI  fastapi a Service"), []string{"fastapi", "service"})
	assert.Equal(t, Words("Who owns payments? (the billing_v2 one)"), []string{"owns", "payments", "billing_v2", "one"})
	e := Entity{Name: "FastAPI backend", Type: "Service"}
	assert.Equal(t, Score(e, Words("fastapi service other")), 2)
}

func TestRecallGate(t *testing.T) {
	for q, skip := range map[string]bool{
		"": true, "  hi! ": true, "Thanks so much": true, "ok.": true, "Good morning": true, "a": true,
		"who owns payments?": false, "what did we decide about Redis": false, "hi, what about the billing service?": false,
	} {
		assert.Equal(t, RecallGate(q) != "", skip, q)
	}
}
