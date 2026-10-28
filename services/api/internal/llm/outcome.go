package llm

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// ErrDeclined is a model's refusal to answer (Anthropic's refusal stop
// reason).
var ErrDeclined = errors.New("the model declined the request")

// outcomeOf is the outcome label of llm_requests_total for err. It looks only
// at the error's type, status code and context error, never at its text,
// which can carry what the provider echoed back.
func outcomeOf(err error) string {
	if err == nil {
		return telemetry.OutcomeOK
	}
	var he *httpError
	if errors.As(err, &he) {
		switch he.status {
		case http.StatusRequestTimeout, http.StatusGatewayTimeout:
			return telemetry.OutcomeTimeout
		case http.StatusTooManyRequests:
			return telemetry.OutcomeRateLimited
		case http.StatusUnauthorized, http.StatusForbidden:
			return telemetry.OutcomeRefused
		}
		return telemetry.OutcomeError
	}
	if errors.Is(err, ErrDeclined) {
		return telemetry.OutcomeRefused
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return telemetry.OutcomeTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return telemetry.OutcomeTimeout
	}
	return telemetry.OutcomeError
}
