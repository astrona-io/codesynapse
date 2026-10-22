package mcpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func transportTotal(t *testing.T, name, tenant string) float64 {
	t.Helper()
	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	var total float64
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(line, name+"{") && (tenant == "" || strings.Contains(line, `tenant_id="`+tenant+`"`)) {
			v, err := strconv.ParseFloat(strings.Fields(line)[1], 64)
			require.NoError(t, err)
			total += v
		}
	}
	return total
}

type partialResponse struct {
	header  http.Header
	flushed bool
}

func (w *partialResponse) Header() http.Header       { return w.header }
func (*partialResponse) WriteHeader(int)             {}
func (*partialResponse) Write(p []byte) (int, error) { return min(4, len(p)), io.ErrClosedPipe }
func (w *partialResponse) Flush()                    { w.flushed = true }

func TestHTTPTransportUsesVerifiedTenantAndPartialBytes(t *testing.T) {
	const tenant = "dddddddddddddddddddddddd"
	ctx := telemetry.WithTenant(t.Context(), "eeeeeeeeeeeeeeeeeeeeeeee")
	beforeIn, beforeOut := transportTotal(t, "net_bytes_received_total", tenant), transportTotal(t, "net_bytes_sent_total", tenant)
	verified := auth.RequireBearerToken(func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: "subject", Extra: map[string]any{ExtraOrgID: tenant}}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	h := verified(MeasureHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.CopyN(io.Discard, r.Body, 3)
		require.NoError(t, err)
		n, err := w.Write([]byte("event: response\n\n"))
		assert.Equal(t, n, 4)
		assert.True(t, errors.Is(err, io.ErrClosedPipe))
		require.NoError(t, http.NewResponseController(w).Flush())
	})))
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", strings.NewReader("unread remainder"))
	req.Header.Set("Authorization", "Bearer valid")
	req.Header.Set("X-Tenant-ID", "ffffffffffffffffffffffff")
	req.Header.Set("X-MCP-Tool", "attacker-tool")
	w := &partialResponse{header: make(http.Header)}
	h.ServeHTTP(w, req)
	assert.True(t, w.flushed)
	assert.Equal(t, transportTotal(t, "net_bytes_received_total", tenant)-beforeIn, 3.0)
	assert.Equal(t, transportTotal(t, "net_bytes_sent_total", tenant)-beforeOut, 4.0)
	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	assert.False(t, strings.Contains(b.String(), "attacker-tool"))
}

type closedWriter struct{}

type failedFlush struct{ *partialResponse }

func (failedFlush) FlushError() error { return io.ErrClosedPipe }

func TestTransportPreservesFlushFailure(t *testing.T) {
	w := &meteredResponse{ResponseWriter: failedFlush{&partialResponse{header: make(http.Header)}}, add: func(int) {}}
	assert.True(t, errors.Is(http.NewResponseController(w).Flush(), io.ErrClosedPipe))
}

func (closedWriter) Write(p []byte) (int, error) { return min(2, len(p)), context.Canceled }
func (closedWriter) Close() error                { return nil }

func TestStdioTransportCountsFramesAsPlatform(t *testing.T) {
	ctx := telemetry.WithTenant(t.Context(), "ffffffffffffffffffffffff")
	beforeIn := transportTotal(t, "platform_net_bytes_received_total", "")
	beforeOut := transportTotal(t, "platform_net_bytes_sent_total", "")
	tr := MeasureStdio(ctx, io.NopCloser(strings.NewReader("{}\n{}\n")), closedWriter{})
	n, err := io.Copy(io.Discard, tr.Reader)
	require.NoError(t, err)
	assert.Equal(t, n, int64(6))
	nw, err := tr.Writer.Write([]byte("{}\n"))
	assert.Equal(t, nw, 2)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, transportTotal(t, "platform_net_bytes_received_total", "")-beforeIn, 6.0)
	assert.Equal(t, transportTotal(t, "platform_net_bytes_sent_total", "")-beforeOut, 2.0)
}
