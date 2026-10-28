package llm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestOutcomeOf(t *testing.T) {
	status := func(code int) error {
		return fmt.Errorf("complete: %w", &httpError{name: "p", status: code, msg: "rate limit exceeded, timeout, forbidden"})
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"no error", nil, "ok"},
		{"request timeout", status(http.StatusRequestTimeout), "timeout"},
		{"gateway timeout", status(http.StatusGatewayTimeout), "timeout"},
		{"rate limited", status(http.StatusTooManyRequests), "rate_limited"},
		{"unauthorised", status(http.StatusUnauthorized), "refused"},
		{"forbidden", status(http.StatusForbidden), "refused"},
		{"server error with telling text", status(http.StatusInternalServerError), "error"},
		{"bad request", status(http.StatusBadRequest), "error"},
		{"model declined", fmt.Errorf("p: %w", ErrDeclined), "refused"},
		{"deadline", fmt.Errorf("p: %w", context.DeadlineExceeded), "timeout"},
		{"network timeout", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, "timeout"},
		{"cancelled", fmt.Errorf("p: %w", context.Canceled), "error"},
		{"text alone never decides", errors.New("HTTP 429: rate limited, timeout"), "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, outcomeOf(tc.err), tc.want)
		})
	}
}

func TestRateLimitedRequestsAreCountedSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	const tenant = "acacacacacacacacacacacac"
	telemetry.AllowModels("limited", "m")
	r := NewRegistry(Config{Providers: []ProviderConfig{{Name: "limited", Kind: KindOpenAI, BaseURL: srv.URL + "/v1", Model: "m", TimeoutSeconds: 5}}, Default: "limited"})
	_, err := r.Complete(telemetry.WithTenant(modelContext(t), tenant), "limited", Request{System: "s", User: "u", MaxTokens: 10})
	require.Error(t, err)

	var out strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&out))
	found := false
	for line := range strings.Lines(out.String()) {
		if strings.HasPrefix(line, `llm_requests_total{tenant_id="`+tenant+`",llm_provider="limited",llm_model="m",`) && strings.Contains(line, `outcome="rate_limited"`) {
			found = true
		}
	}
	assert.True(t, found, "a 429 is counted as rate_limited, not error")
}
