package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/astrona-io/codesynapse/internal/app"
)

// One outage mark per machine and API URL, shared by the hooks of every chat
// and by the bridges. The per-chat offline marker (offlineMarker) is written
// only by a bridge that found the server down, so a new chat, or a hook that
// runs before its bridge noticed, paid the full timeout of every request.
// Whoever fails to connect now writes this mark, every hook skips the server
// while it is fresh, and any answer from the server clears it. It holds a
// time and a reason class, never the URL (the file name is a hash of it), a
// session or a path. A refusal (401/403) is not an outage: it stays the
// chat's own business (offlineMarker.Refused).

const (
	// hookDialTimeout and hookTLSTimeout bound connecting for a hook, so a
	// dead host or dropped packets cost one short wait, not the request's
	// whole budget.
	hookDialTimeout = 1200 * time.Millisecond
	hookTLSTimeout  = 1500 * time.Millisecond
	// hookRequestTimeout bounds one hook request that has no tighter budget.
	hookRequestTimeout = 8 * time.Second
	// outageFresh is how long hooks skip the server after a failed connect.
	outageFresh = 2 * time.Minute
	// maxMarkBytes bounds every marker file read.
	maxMarkBytes = 4096
)

// outageReasons are the reason classes a mark may carry, with their text
// for doctor. Anything else in a file makes it invalid.
var outageReasons = map[string]string{
	"refused":      "connection refused",
	"dns":          "the host name did not resolve",
	"dial-timeout": "connecting timed out",
	"dial":         "could not connect",
	"tls":          "the TLS handshake failed",
	"gateway":      "a proxy answered 502, 503 or 504",
}

type outageMark struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// age is how old the mark is at now.
func (m outageMark) age(now time.Time) time.Duration { return now.Sub(m.At) }

// fresh reports whether hooks skip the server at now. A time ahead of the
// clock (a damaged file, a clock set back) must not keep them off for good.
func (m outageMark) fresh(now time.Time) bool {
	a := m.age(now)
	return a < outageFresh && a > -time.Minute
}

// parseOutageMark reads a mark defensively: bounded, a known reason and a
// set time, or nothing.
func parseOutageMark(data []byte) (outageMark, bool) {
	if len(data) == 0 || len(data) > maxMarkBytes {
		return outageMark{}, false
	}
	var m outageMark
	if json.Unmarshal(data, &m) != nil || m.At.IsZero() {
		return outageMark{}, false
	}
	if _, ok := outageReasons[m.Reason]; !ok {
		return outageMark{}, false
	}
	return m, true
}

// offlineDir holds the outage marks and the per-chat markers.
func offlineDir() string {
	dir, err := bridgeCacheRoot()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "codesynapse", "offline")
}

// outageFile is the mark of the API at base. The URL never appears in the
// name, only a hash of it.
func outageFile(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	dir := offlineDir()
	if base == "" || dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(base))
	return filepath.Join(dir, "server-"+hex.EncodeToString(sum[:16])+".json")
}

// readSmall reads a marker file of ours, refusing one larger than
// maxMarkBytes.
func readSmall(file string) []byte {
	if file == "" {
		return nil
	}
	f, err := os.Open(file) //nolint:gosec // our own marker
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(f, maxMarkBytes+1))
	if err != nil || len(data) > maxMarkBytes {
		return nil
	}
	return data
}

// readOutage returns the mark of the API at base, fresh or not.
func readOutage(base string) (outageMark, bool) {
	return parseOutageMark(readSmall(outageFile(base)))
}

// serverDown reports whether a fresh mark says the API at base could not be
// reached a moment ago.
func serverDown(base string) bool {
	m, ok := readOutage(base)
	return ok && m.fresh(time.Now())
}

// proxyDown reports whether res is a front proxy's answer for a server behind
// it that is down. The API answers its own 502, 503 and 504 (an audit entry
// it could not write, a store that is down) as JSON problem documents: it is
// there, so that is no outage.
func proxyDown(res *http.Response) bool {
	return gatewayStatus(res.StatusCode) && !strings.Contains(strings.ToLower(res.Header.Get("Content-Type")), "json")
}

// markOutage records that connecting to the API at base failed for reason.
func markOutage(base, reason string) {
	if _, ok := outageReasons[reason]; !ok {
		return
	}
	file := outageFile(base)
	if file == "" {
		return
	}
	if data, err := json.Marshal(outageMark{At: time.Now().UTC(), Reason: reason}); err == nil {
		writePrivate(file, data)
	}
}

// noteOutage marks the API at base down when err is a connection failure;
// any other error leaves the mark as it is.
func noteOutage(base string, err error) {
	if reason := connectFailure(err); reason != "" {
		markOutage(base, reason)
	}
}

// clearOutage removes the mark of the API at base: it answered.
func clearOutage(base string) {
	if file := outageFile(base); file != "" {
		_ = os.Remove(file) //nolint:errcheck // absent is the goal
	}
}

// connectFailure classifies err as a failure to reach the server at all, ""
// otherwise. A server that accepted the connection and is slow to answer is
// not one: its requests are bounded by their own budgets. A caller that gave
// up (context.Canceled) says nothing about the server.
func connectFailure(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	var se *statusError
	if errors.As(err, &se) {
		if gatewayStatus(se.Status) {
			return "gateway"
		}
		return ""
	}
	var mse *mcpStatusError
	if errors.As(err, &mse) {
		if gatewayStatus(mse.Status) {
			return "gateway"
		}
		return ""
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		if op.Timeout() {
			return "dial-timeout"
		}
		return "dial"
	}
	var rh tls.RecordHeaderError
	var cv *tls.CertificateVerificationError
	var alert tls.AlertError
	if errors.As(err, &rh) || errors.As(err, &cv) || errors.As(err, &alert) ||
		strings.Contains(err.Error(), "TLS handshake timeout") { // net/http's own, unexported type
		return "tls"
	}
	return ""
}

// newHookHTTP is the HTTP client of a hook: short connect and handshake
// timeouts, the request bounded by timeout.
func newHookHTTP(timeout time.Duration) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own type
	t.DialContext = (&net.Dialer{Timeout: hookDialTimeout, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = hookTLSTimeout
	return &http.Client{Transport: t, Timeout: timeout}
}

// newHookClient is the API client of a hook: it connects with the short
// budget and does not try while the outage mark of base is fresh
// (errServerOffline).
func newHookClient(base string) apiClient {
	c := newAPIClient(base)
	c.http = newHookHTTP(hookRequestTimeout)
	c.hook = true
	return c
}

// hookAPIURL is the API the hooks talk to.
func hookAPIURL() string {
	return app.Getenv("CODESYNAPSE_API_URL", "http://127.0.0.1:8080")
}

// sameOrigin reports whether a and b name the same scheme, host and port.
func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Host != "" &&
		strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

// sameHostname reports whether a and b name the same host, whatever the port.
func sameHostname(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	return err1 == nil && err2 == nil && ua.Hostname() != "" && strings.EqualFold(ua.Hostname(), ub.Hostname())
}

// shareOutage records a connection failure of the bridge's MCP endpoint in
// the API's outage mark when it says something about the API: the same
// origin, or the same host when the name did not resolve or connecting
// timed out (a host that is gone, not one port that is closed).
func (b *bridge) shareOutage(err error) {
	api := b.root().report.base
	reason := connectFailure(err)
	if api == "" || reason == "" {
		return
	}
	if sameOrigin(b.url, api) || (sameHostname(b.url, api) && (reason == "dns" || reason == "dial-timeout")) {
		markOutage(api, reason)
	}
}

// shareOnline clears the API's outage mark when the MCP endpoint that just
// answered is the same origin.
func (b *bridge) shareOnline() {
	if api := b.root().report.base; api != "" && sameOrigin(b.url, api) {
		clearOutage(api)
	}
}
