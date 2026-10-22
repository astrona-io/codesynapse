package mcpserver

import (
	"context"
	"io"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
)

// MeasureHTTP runs inside bearer authentication. Framing, notifications and
// multiplexed responses use one fixed transport category, never tool arguments.
func MeasureHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := telemetry.WithTenant(r.Context(), "")
		if info := auth.TokenInfoFromContext(r.Context()); info != nil {
			if tenant, ok := info.Extra[ExtraOrgID].(string); ok {
				ctx = telemetry.WithTenant(ctx, tenant)
			}
		}
		ctx = telemetry.WithTool(ctx, "transport")
		ctx = telemetry.WithModel(ctx, "none", "none")
		r = r.WithContext(ctx)
		if r.Body != nil {
			r.Body = &meteredReader{ReadCloser: r.Body, add: func(n int) {
				telemetry.NetBytesReceived.AddContext(ctx, float64(n), telemetry.Tenant(ctx), "transport")
			}}
		}
		next.ServeHTTP(&meteredResponse{ResponseWriter: w, add: func(n int) {
			telemetry.NetBytesSent.AddContext(ctx, float64(n), telemetry.Tenant(ctx), "transport")
		}}, r)
	})
}

type meteredReader struct {
	io.ReadCloser
	add func(int)
}

func (r *meteredReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.add(n)
	}
	return n, err
}

type meteredResponse struct {
	http.ResponseWriter
	add func(int)
}

func (w *meteredResponse) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.add(n)
	}
	return n, err
}
func (w *meteredResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *meteredResponse) Flush()                      { _ = w.FlushError() }
func (w *meteredResponse) FlushError() error {
	return http.NewResponseController(w.ResponseWriter).Flush()
}

type meteredWriter struct {
	io.WriteCloser
	add func(int)
}

func (w *meteredWriter) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	if n > 0 {
		w.add(n)
	}
	return n, err
}

// MeasureStdio counts protocol bytes, including newline frames. Reads may contain
// several requests before their authentication, so these are platform totals.
func MeasureStdio(ctx context.Context, reader io.ReadCloser, writer io.WriteCloser) *mcp.IOTransport {
	ctx = telemetry.WithTenant(ctx, "")
	ctx = telemetry.WithTool(ctx, "transport")
	ctx = telemetry.WithModel(ctx, "none", "none")
	return &mcp.IOTransport{
		Reader: &meteredReader{ReadCloser: reader, add: func(n int) {
			telemetry.NetBytesReceived.AddContext(ctx, float64(n), telemetry.Tenant(ctx), "transport")
		}},
		Writer: &meteredWriter{WriteCloser: writer, add: func(n int) { telemetry.NetBytesSent.AddContext(ctx, float64(n), telemetry.Tenant(ctx), "transport") }},
	}
}
