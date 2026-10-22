package llm

import (
	"io"
	"net/http"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

type measuredTransport struct{ next http.RoundTripper }

func modelTransport(cfg ProviderConfig, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if cfg.Local() {
		return next
	}
	return measuredTransport{next: next}
}

// The transport reads each attempt's encoded body, including SDK retries.
// Count application bytes before HTTP framing, without retaining the payload.
func (t measuredTransport) RoundTrip(req *http.Request) (*http.Response, error) { //nolint:contextcheck // body callbacks retain req.Context
	if req.Body != nil {
		req = req.Clone(req.Context())
		ctx := req.Context()
		add := func(n int) {
			telemetry.CloudBytesSent.AddContext(ctx, float64(n), telemetry.Tenant(ctx), "", "")
		}
		req.Body = measureBody(req.Body, add)
		if getBody := req.GetBody; getBody != nil {
			req.GetBody = func() (io.ReadCloser, error) {
				body, err := getBody()
				if err != nil {
					return nil, err
				}
				return measureBody(body, add), nil
			}
		}
	}
	return t.next.RoundTrip(req)
}

func measureBody(body io.ReadCloser, add func(int)) io.ReadCloser {
	// Redirects can hand a body from the already measured GetBody back to
	// RoundTrip; reading that body must still count only once.
	if _, measured := body.(*measuredBody); measured {
		return body
	}
	return &measuredBody{ReadCloser: body, add: add}
}

type measuredBody struct {
	io.ReadCloser
	add func(int)
}

func (r *measuredBody) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.add(n)
	}
	return n, err
}
