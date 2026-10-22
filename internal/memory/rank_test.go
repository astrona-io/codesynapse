package memory

import (
	"fmt"
	"math"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestTokens(t *testing.T) {
	assert.Equal(t, Tokens("PaymentsAPI v2"), []string{"paymentsapi", "payments", "api", "v2", "v", "2"})
	assert.Equal(t, Tokens("user_1 checkout-service"), []string{"user_1", "user", "1", "checkout-service", "checkout", "service"})
	assert.Equal(t, Tokens("Team Blue"), []string{"team", "blue"})
}

func names(ss []Scored) []string {
	out := []string{}
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestRankBM25(t *testing.T) {
	ents := []Entity{
		{ID: "1", Name: "Payments service"},
		{ID: "2", Name: "Billing service"},
		{ID: "3", Name: "Search service"},
		{ID: "4", Name: "Frontend", Aliases: []string{"web app"}},
		{ID: "5", Name: "CheckoutHandler", Type: "Component"},
	}
	t.Run("rare terms weigh more", func(t *testing.T) {
		got := RankBM25(ents, "payments service", 10)
		require.Len(t, got, 3, "every service matches")
		assert.Equal(t, got[0].Name, "Payments service", "payments is rare, service is everywhere")
	})
	t.Run("aliases, types, camelCase and prefixes", func(t *testing.T) {
		assert.Equal(t, names(RankBM25(ents, "web", 10)), []string{"Frontend"})
		assert.Equal(t, names(RankBM25(ents, "component", 10)), []string{"CheckoutHandler"})
		assert.Equal(t, names(RankBM25(ents, "checkout handler", 10)), []string{"CheckoutHandler"})
		assert.Equal(t, names(RankBM25(ents, "pay", 10)), []string{"Payments service"}, "prefix from three letters")
		assert.Len(t, RankBM25(ents, "pa", 10), 0, "but not shorter")
	})
	t.Run("nothing to match", func(t *testing.T) {
		assert.Len(t, RankBM25(ents, "", 10), 0)
		assert.Len(t, RankBM25(ents, "the and of", 10), 0, "stopwords only")
		assert.Len(t, RankBM25(ents, "kubernetes", 10), 0)
		assert.Len(t, RankBM25(nil, "payments", 10), 0)
	})
	t.Run("k bounds", func(t *testing.T) {
		assert.Len(t, RankBM25(ents, "service", 2), 2)
		assert.Len(t, RankBM25(ents, "service", 0), 0)
	})
}

func TestFuseRRF(t *testing.T) {
	e := func(id string) Scored { return Scored{Entity: Entity{ID: id, Name: id}} }
	got := FuseRRF(10, map[string][]Scored{
		"vector":  {e("a"), e("b"), e("c")},
		"lexical": {e("b"), e("d"), e("b")},
	})
	require.Len(t, got, 4, "deduplicated")
	assert.Equal(t, got[0].ID, "b", "found by both legs beats first place in one")
	assert.Equal(t, got[0].Legs, []string{"lexical", "vector"})
	assert.Equal(t, got[1].ID, "a")
	assert.True(t, math.Abs(got[0].Score-(1.0/61+1.0/62)) < 1e-12, "the duplicate in one list counts once")
	assert.Len(t, FuseRRF(2, map[string][]Scored{"x": {e("a"), e("b"), e("c")}}), 2)
	assert.Len(t, FuseRRF(5, nil), 0)
}

func BenchmarkRankBM25Full(b *testing.B) {
	words := []string{"payments", "billing", "search", "checkout", "ledger", "gateway", "team", "service", "handler", "api", "frontend", "worker"}
	ents := make([]Entity, MaxEntitiesPerRepo)
	for i := range ents {
		ents[i] = Entity{ID: fmt.Sprint(i), Name: fmt.Sprintf("%s %s %d", words[i%len(words)], words[(i/7)%len(words)], i), Aliases: []string{words[(i/3)%len(words)] + "Alias"}}
	}
	b.ResetTimer()
	for range b.N {
		RankBM25(ents, "payments gateway retries", 8)
	}
}
