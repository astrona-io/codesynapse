package fga

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

const (
	testStore = "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	testModel = "01ARZ3NDEKTSV4RRFFQ69G5FAW"
)

func TestConfigURL(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://fga.example.com", true},
		{"https://fga.example.com:8443/", true},
		{"http://openfga:8080", true},
		{"http://openfga.codesynapse.svc.cluster.local:8080", true},
		{"http://localhost:8080", true},
		{"http://fga.example.com", false},
		{"http://10.0.0.5:8080", false},
		{"http://127.0.0.1:8080", false},
		{"https://user:pw@fga.example.com", false},
		{"https://fga.example.com/prefix", false},
		{"https://fga.example.com?x=1", false},
		{"ftp://openfga", false},
		{"openfga:8080", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			err := Config{URL: tc.url, Token: "k"}.validate()
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrURL)
			}
		})
	}
	assert.ErrorIs(t, Config{URL: "http://openfga:8080"}.validate(), ErrNoToken)
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvURL, "")
	t.Setenv(EnvToken, "")
	_, err := ConfigFromEnv()
	assert.ErrorIs(t, err, ErrNotConfigured)

	t.Setenv(EnvURL, "http://openfga:8080")
	_, err = ConfigFromEnv()
	assert.ErrorIs(t, err, ErrNoToken)

	t.Setenv(EnvToken, "secret")
	cfg, err := ConfigFromEnv()
	require.NoError(t, err)
	assert.Equal(t, cfg.Token, "secret")
}

// fake answers OpenFGA's HTTP API from a handler per path suffix.
func fake(t *testing.T, handlers map[string]http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		for suffix, h := range handlers {
			if strings.HasSuffix(r.URL.Path, suffix) {
				h(w, r)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	// localhost passes the URL rule; the test server listens on loopback.
	url := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	c, err := New(Config{URL: url, Token: "secret", StoreID: testStore, ModelID: testModel, Timeout: 300 * time.Millisecond})
	require.NoError(t, err)
	return c
}

func answer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

var anyTuple = Tuple{User: "user:u", Relation: "read", Object: "repository:r"}

func TestCheckFailsClosed(t *testing.T) {
	ctx := context.Background()
	for name, h := range map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"bad request":  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
		"no decision":  answer(`{}`),
		"not json":     answer(`allowed`),
		"too slow": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			answer(`{"allowed":true}`)(w, r)
		},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere/check", http.StatusTemporaryRedirect)
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake(t, map[string]http.HandlerFunc{"/check": h, "/elsewhere/check": answer(`{"allowed":true}`)})
			ok, err := c.Check(ctx, anyTuple)
			assert.Error(t, err)
			assert.False(t, ok)
		})
	}
}

func TestCheckAsksForHigherConsistency(t *testing.T) {
	var body map[string]any
	c := fake(t, map[string]http.HandlerFunc{"/check": func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		answer(`{"allowed":true}`)(w, r)
	}})
	ok, err := c.Check(context.Background(), anyTuple)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, body["consistency"], any("HIGHER_CONSISTENCY"))
	assert.Equal(t, body["authorization_model_id"], any(testModel))
}

func TestNoStoreFailsClosed(t *testing.T) {
	c, err := New(Config{URL: "http://openfga:8080", Token: "k"})
	require.NoError(t, err)
	ok, err := c.Check(context.Background(), anyTuple)
	assert.ErrorIs(t, err, ErrNoStore)
	assert.False(t, ok)
	_, err = c.ListObjects(context.Background(), "user:u", "read", TypeRepository)
	assert.ErrorIs(t, err, ErrNoStore)
}

func TestBatchCheckFailsClosed(t *testing.T) {
	ctx := context.Background()
	checks := []Tuple{anyTuple, anyTuple, anyTuple}

	ok := fake(t, map[string]http.HandlerFunc{"/batch-check": answer(`{"result":{"0":{"allowed":true},"1":{"allowed":false},"2":{"allowed":true}}}`)})
	got, err := ok.BatchCheck(ctx, checks)
	require.NoError(t, err)
	assert.Equal(t, got, []bool{true, false, true})

	for name, body := range map[string]string{
		"one check failed": `{"result":{"0":{"allowed":true},"1":{"error":{"message":"boom"}},"2":{"allowed":true}}}`,
		"one missing":      `{"result":{"0":{"allowed":true},"2":{"allowed":true}}}`,
		"no result":        `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := fake(t, map[string]http.HandlerFunc{"/batch-check": answer(body)})
			got, err := c.BatchCheck(ctx, checks)
			assert.Error(t, err)
			assert.Equal(t, got, []bool{false, false, false})
		})
	}

	got, err = ok.BatchCheck(ctx, make([]Tuple, MaxBatchCheck+1))
	assert.ErrorIs(t, err, ErrTooMany)
	assert.Len(t, got, MaxBatchCheck+1)
	for _, g := range got {
		assert.False(t, g)
	}
}

func TestListObjectsRefusesTruncation(t *testing.T) {
	objs := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf(`"repository:r%d"`, i)
		}
		return `{"objects":[` + strings.Join(parts, ",") + `]}`
	}
	ctx := context.Background()

	c := fake(t, map[string]http.HandlerFunc{"/list-objects": answer(objs(3))})
	got, err := c.ListObjects(ctx, "user:u", "read", TypeRepository)
	require.NoError(t, err)
	assert.Len(t, got, 3)

	c = fake(t, map[string]http.HandlerFunc{"/list-objects": answer(objs(ListLimit))})
	got, err = c.ListObjects(ctx, "user:u", "read", TypeRepository)
	assert.ErrorIs(t, err, ErrTruncated)
	assert.Len(t, got, 0)

	c = fake(t, map[string]http.HandlerFunc{"/list-objects": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }})
	got, err = c.ListObjects(ctx, "user:u", "read", TypeRepository)
	assert.Error(t, err)
	assert.Len(t, got, 0)
}

func TestWriteBounds(t *testing.T) {
	var calls int
	c := fake(t, map[string]http.HandlerFunc{"/write": func(w http.ResponseWriter, r *http.Request) {
		calls++
		answer(`{}`)(w, r)
	}})
	ctx := context.Background()
	assert.ErrorIs(t, c.WriteTuples(ctx, make([]Tuple, MaxWrite+1)), ErrTooMany)
	assert.ErrorIs(t, c.DeleteTuples(ctx, make([]Tuple, MaxWrite+1)), ErrTooMany)
	assert.NoError(t, c.WriteTuples(ctx, nil))
	assert.Equal(t, calls, 0)
	assert.NoError(t, c.WriteTuples(ctx, []Tuple{anyTuple}))
	assert.Equal(t, calls, 1)
}

func TestReadPagesAndBounds(t *testing.T) {
	page := func(token string, objs ...string) string {
		parts := make([]string, len(objs))
		for i, o := range objs {
			parts[i] = fmt.Sprintf(`{"key":{"user":"user:u","relation":"member","object":%q}}`, o)
		}
		return `{"tuples":[` + strings.Join(parts, ",") + `],"continuation_token":"` + token + `"}`
	}
	var bodies []map[string]any
	c := fake(t, map[string]http.HandlerFunc{"/read": func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if body["continuation_token"] == "next" {
			answer(page("", "team:b"))(w, r)
			return
		}
		answer(page("next", "team:a"))(w, r)
	}})
	ctx := context.Background()
	got, err := c.Read(ctx, Tuple{Object: "team:a"}, 10)
	require.NoError(t, err)
	assert.Equal(t, got, []Tuple{{User: "user:u", Relation: "member", Object: "team:a"}, {User: "user:u", Relation: "member", Object: "team:b"}})
	require.Len(t, bodies, 2)
	assert.Equal(t, bodies[0]["consistency"], any("HIGHER_CONSISTENCY"))
	assert.Equal(t, bodies[0]["tuple_key"], any(map[string]any{"object": "team:a"}))

	got, err = c.Read(ctx, Tuple{Object: "team:a"}, 1)
	assert.ErrorIs(t, err, ErrTruncated)
	assert.Len(t, got, 0)

	c = fake(t, map[string]http.HandlerFunc{"/read": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }})
	got, err = c.Read(ctx, Tuple{Object: "team:a"}, 10)
	assert.Error(t, err)
	assert.Len(t, got, 0)
}
