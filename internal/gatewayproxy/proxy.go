package gatewayproxy

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DirectUpstream is the only address a request goes to when the gateway
	// cannot be reached. Requests cannot change it.
	DirectUpstream = "https://api.anthropic.com"
	// MaxBody caps a request body, which is held in memory so it can be sent
	// again on the direct route; the same cap as the gateway's.
	MaxBody = 32 << 20
	// DefaultIdle is how long the proxy runs without a request; the next
	// session's hook starts it again.
	DefaultIdle = 2 * time.Hour
	// DefaultCooldown is how long requests go direct before the gateway is
	// tried again.
	DefaultCooldown = 30 * time.Second
	// ReportPath is the gateway endpoint the outage counts go to.
	ReportPath = "/v1/codesynapse/failover"
	// RouteHeader tells the client where an answer came from; the gateway
	// sets it to local or cloud, the proxy to direct.
	RouteHeader = "X-CodeSynapse-Route"

	keyHeader   = "X-CodeSynapse-Key"
	keyPrefix   = "csk_"
	dialTimeout = 5 * time.Second
	tlsTimeout  = 10 * time.Second
	probeWait   = 5 * time.Second
	reportEvery = time.Minute
	// writeWait is how long the gateway may take to accept the request. A
	// request it never took in cannot have been acted on, so it goes direct.
	writeWait = time.Minute
	// headerWait is how long the gateway may take to answer a request it
	// took in. A non-streamed answer comes only when the model finished, so
	// it is long; past it the request fails (it may have been acted on) and
	// later requests go direct.
	headerWait = 10 * time.Minute
)

// directRequest are the only headers sent to Anthropic, as on the gateway's
// own cloud leg: X-CodeSynapse-* and everything else stay behind, so the key
// never reaches Anthropic.
var directRequest = []string{
	"Content-Type", "Accept", "Authorization", "X-Api-Key",
	"Anthropic-Version", "Anthropic-Beta", "User-Agent",
}

// hopByHop headers are never passed on in either direction.
var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Options configure a Proxy.
type Options struct {
	Config Config
	Files  Files
	Secret []byte
	Logger *slog.Logger
	// Idle and Cooldown default to DefaultIdle and DefaultCooldown.
	Idle, Cooldown time.Duration
}

// Proxy forwards Claude Code's model requests to the gateway and fails over
// to Anthropic while the gateway is unreachable.
type Proxy struct {
	cfg      Config
	files    Files
	gateway  *url.URL
	secret   []byte
	log      *slog.Logger
	idle     time.Duration
	cooldown time.Duration
	// direct is DirectUpstream; only tests in this package change it.
	direct string
	// tick is how often Run checks idleness and its config.
	tick time.Duration
	// writeWait is writeWait; only tests in this package change it.
	writeWait time.Duration

	gw, out *http.Client
	counts  *counts

	inflight atomic.Int64
	lastUse  atomic.Int64

	mu         sync.Mutex
	ctx        context.Context
	down       bool
	key        string
	reporting  bool
	nextReport time.Time
}

// New returns a proxy for the saved config.
func New(o Options) (*Proxy, error) {
	if err := o.Config.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(o.Config.Gateway, "/"))
	if err != nil {
		return nil, err
	}
	if len(o.Secret) == 0 {
		return nil, errors.New("no proxy secret")
	}
	p := &Proxy{
		cfg: o.Config, files: o.Files, gateway: u, secret: o.Secret,
		log: o.Logger, idle: o.Idle, cooldown: o.Cooldown,
		direct: DirectUpstream, tick: time.Minute, writeWait: writeWait, ctx: context.Background(),
		counts: openCounts(o.Files.countsPath()),
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.idle <= 0 {
		p.idle = DefaultIdle
	}
	if p.cooldown <= 0 {
		p.cooldown = DefaultCooldown
	}
	// HTTPS_PROXY/NO_PROXY from the environment apply to both legs, as they
	// did when Claude Code talked to the gateway and Anthropic itself; a
	// request can never choose a proxy or an address.
	p.gw = newClient(30*time.Second, headerWait)
	p.out = newClient(30*time.Second, 0)
	p.lastUse.Store(time.Now().UnixNano())
	return p, nil
}

func newClient(idle, header time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   tlsTimeout,
			IdleConnTimeout:       idle,
			ResponseHeaderTimeout: header,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Down says whether requests currently go direct.
func (p *Proxy) Down() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.down
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || !loopbackHost(r.Host) {
		// A web page in the developer's browser can reach 127.0.0.1 too; it
		// must not plant a key here, use the proxy or read the health proof.
		// Claude Code sends neither.
		problem(w, http.StatusForbidden, "the CodeSynapse gateway proxy does not serve browsers")
		return
	}
	if r.URL.Path == HealthPath {
		p.health(w, r)
		return
	}
	p.inflight.Add(1)
	defer func() {
		p.lastUse.Store(time.Now().UnixNano())
		p.inflight.Add(-1)
	}()
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		problem(w, http.StatusNotFound, "the CodeSynapse gateway proxy serves /v1/ only")
		return
	}
	if !p.takeKey(r.Header.Get(keyHeader)) {
		problem(w, http.StatusForbidden, "this CodeSynapse gateway proxy was set up for another CodeSynapse key; run codesynapse mcp install --gateway again")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		problem(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	if p.Down() {
		p.sendDirect(w, r, body, "the CodeSynapse gateway is unreachable")
		return
	}
	switch p.toGateway(w, r, body) {
	case sent:
		p.maybeReport() //nolint:contextcheck // the report outlives the request; it runs under the proxy's context
	case unreachable:
		p.markDown()
		p.sendDirect(w, r, body, "the CodeSynapse gateway is unreachable")
	case hung:
		// This request was answered with an error; the next ones go direct.
		p.markDown()
	}
}

// takeKey checks the request's CodeSynapse key against the one this proxy
// holds for probes and reports: the key install pinned by its digest, or
// else the first one a request brought. A request with another key is
// refused, so no local program can make the proxy report under its key.
// A request without one passes; the gateway refuses it itself.
func (p *Proxy) takeKey(k string) bool {
	if k == "" {
		return true
	}
	if !strings.HasPrefix(k, keyPrefix) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.key != "":
		return subtle.ConstantTimeCompare([]byte(k), []byte(p.key)) == 1
	case p.cfg.KeyHash != "" && subtle.ConstantTimeCompare([]byte(HashKey(k)), []byte(p.cfg.KeyHash)) != 1:
		return false
	}
	p.key = k
	return true
}

// loopbackHost refuses a Host header that names another site, which is how a
// DNS-rebinding page would reach the proxy.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type outcome int

const (
	sent        outcome = iota // the gateway answered (any status) or the request failed after it was delivered
	unreachable                // nothing reached the gateway: safe to send elsewhere
	hung                       // the gateway took the request and never answered: failed, not resent
)

// toGateway forwards the request. It returns unreachable only when nothing
// was written to the client and the gateway cannot have acted on the
// request: the connection or TLS failed before the request was written, or
// a front proxy answered 502/503/504 with a body that is not the gateway's
// JSON. Everything else, 401 and 429 included, is the gateway's answer and
// goes to the client as it is.
func (p *Proxy) toGateway(w http.ResponseWriter, r *http.Request, body []byte) outcome {
	u := *p.gateway
	u.Path = strings.TrimRight(u.Path, "/") + r.URL.Path
	u.RawQuery = r.URL.RawQuery
	var wrote atomic.Bool
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// A gateway that accepts the connection but never takes the request in
	// is given up on before anything was written, so the request may still
	// go direct. Once it was written, only the header timeout applies.
	stall := time.AfterFunc(p.writeWait, func() {
		if !wrote.Load() {
			cancel()
		}
	})
	defer stall.Stop()
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(i httptrace.WroteRequestInfo) {
			if i.Err == nil {
				wrote.Store(true)
			}
		},
	})
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		problem(w, http.StatusBadGateway, "could not build the gateway request")
		return sent
	}
	copyHeaders(req.Header, r.Header)
	resp, err := p.gw.Do(req) //nolint:gosec // G704: the gateway URL is the validated install config
	if err != nil {
		if r.Context().Err() != nil {
			return sent // the client went away; nothing to answer
		}
		if !wrote.Load() {
			p.log.Warn("gateway unreachable", "error", errClass(err))
			return unreachable
		}
		// The gateway may have started on it: never send it twice.
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			p.log.Warn("gateway did not answer", "error", errClass(err))
			problem(w, http.StatusGatewayTimeout, "the CodeSynapse gateway did not answer the request")
			return hung
		}
		problem(w, http.StatusBadGateway, "the CodeSynapse gateway broke off the request")
		return sent
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	if frontProxyDown(resp) {
		p.log.Warn("gateway unreachable", "status", resp.StatusCode)
		return unreachable
	}
	stream(w, resp, "")
	return sent
}

// frontProxyDown is a 502, 503 or 504 that did not come from the gateway: a
// load balancer or ingress in front of a stopped API answers HTML or plain
// text. The gateway marks every answer of its own with RouteHeader, also a
// status it relays from Anthropic (which may be an HTML 503 from Anthropic's
// edge after the request was sent upstream): that is never a reason to send
// the request again.
func frontProxyDown(resp *http.Response) bool {
	if resp.Header.Get(RouteHeader) != "" {
		return false
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json")
	}
	return false
}

// directPaths are what Anthropic's API serves and the gateway passes on.
func directPath(p string) bool { return p == "/v1/messages" || p == "/v1/messages/count_tokens" }

// directURL is the Anthropic URL for a request directPath accepted. It is
// built from constants only: nothing the caller sent becomes part of it
// except the choice of path and Claude Code's beta=true flag.
func (p *Proxy) directURL(r *http.Request) string {
	path := "/v1/messages"
	if r.URL.Path == "/v1/messages/count_tokens" {
		path = "/v1/messages/count_tokens"
	}
	u := strings.TrimSuffix(p.direct, "/") + path
	if r.URL.Query().Get("beta") == "true" {
		u += "?beta=true"
	}
	return u
}

// sendDirect sends the request to Anthropic with the caller's own credential
// and counts it.
func (p *Proxy) sendDirect(w http.ResponseWriter, r *http.Request, body []byte, why string) {
	if r.Method != http.MethodPost || !directPath(r.URL.Path) {
		problem(w, http.StatusBadGateway, why)
		return
	}
	for _, h := range []string{"Authorization", "X-Api-Key"} {
		if strings.Contains(r.Header.Get(h), keyPrefix) {
			// A CodeSynapse key must never reach Anthropic.
			problem(w, http.StatusBadGateway, why+"; your model credential is a CodeSynapse key, so the request was not sent to Anthropic")
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.directURL(r), bytes.NewReader(body)) //nolint:gosec // G704: the fixed Anthropic URL plus a constant path
	if err != nil {
		problem(w, http.StatusBadGateway, why)
		return
	}
	for _, h := range directRequest {
		if v := r.Header.Values(h); len(v) > 0 {
			req.Header[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		}
	}
	resp, err := p.out.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		if r.Context().Err() == nil {
			p.log.Warn("direct route failed too", "error", errClass(err))
			problem(w, http.StatusBadGateway, why+", and Anthropic could not be reached either")
		}
		return
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	p.countDirect(requestModel(body))
	stream(w, resp, "direct")
}

// countDirect counts a request Anthropic answered. It holds p.mu, like
// markDown and the probe that ends an outage, so the count lands in the
// window of the outage it belongs to; one that finished after the outage
// ended gets a closed window of its own (counts.direct), never an open one.
func (p *Proxy) countDirect(model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts.direct(time.Now(), model)
}

// requestModel reads only the model name; the body is never kept or logged.
func requestModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &v) //nolint:errcheck // a bad body counts as "other"
	return v.Model
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	for _, f := range src.Values("Connection") {
		for _, h := range strings.Split(f, ",") {
			dst.Del(strings.TrimSpace(h))
		}
	}
	for _, h := range hopByHop {
		dst.Del(h)
	}
	dst.Del("Host")
	dst.Del("Content-Length")
}

// stream copies the answer as it arrives, flushing each read so server-sent
// events reach the client without delay. A failure mid-stream ends the
// answer; it is never retried.
func stream(w http.ResponseWriter, resp *http.Response, route string) {
	h := w.Header()
	for k, v := range resp.Header {
		h[k] = append([]string(nil), v...)
	}
	for _, x := range hopByHop {
		h.Del(x)
	}
	if route != "" {
		h.Set(RouteHeader, route)
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// errClass names a transport failure without its text, which can carry
// addresses the log does not need.
func errClass(err error) string {
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &dns):
		return "dns"
	case strings.Contains(err.Error(), "tls:") || strings.Contains(err.Error(), "x509:"):
		return "tls"
	case errors.As(err, &op):
		return op.Op
	}
	return "transport"
}

// markDown opens the breaker: requests go direct until a probe after the
// cool-down finds the gateway again.
func (p *Proxy) markDown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		return
	}
	p.down = true
	p.counts.open(time.Now())
	go p.probeLoop(p.ctx)
}

func (p *Proxy) probeLoop(ctx context.Context) {
	t := time.NewTimer(p.cooldown)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if p.probe(ctx) {
			p.mu.Lock()
			p.down = false
			p.counts.close(time.Now())
			p.nextReport = time.Time{}
			p.mu.Unlock()
			p.log.Info("gateway reachable again")
			p.maybeReport() //nolint:contextcheck // runs under the proxy's context
			return
		}
		t.Reset(p.cooldown)
	}
}

// probe asks GET /v1/models with the key, like CheckGateway. Any answer of
// the gateway itself counts: a 401 or 403 must reach Claude Code from the
// gateway, not be avoided by going direct.
func (p *Proxy) probe(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.gateway.JoinPath("/v1/models").String(), nil)
	if err != nil {
		return false
	}
	p.mu.Lock()
	key := p.key
	p.mu.Unlock()
	if key != "" {
		req.Header.Set(keyHeader, key)
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := p.gw.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return !frontProxyDown(resp)
}

// maybeReport sends closed outage windows to the gateway, at most once a
// minute and never two at a time. It needs the key, which arrives with the
// first request after a restart.
func (p *Proxy) maybeReport() {
	p.mu.Lock()
	now := time.Now()
	if p.down || p.reporting || p.key == "" || now.Before(p.nextReport) {
		p.mu.Unlock()
		return
	}
	pending := p.counts.pending(now)
	if len(pending) == 0 {
		p.mu.Unlock()
		return
	}
	p.reporting, p.nextReport = true, now.Add(reportEvery)
	key, ctx := p.key, p.ctx
	p.mu.Unlock()
	go func() {
		defer func() {
			p.mu.Lock()
			p.reporting = false
			p.mu.Unlock()
		}()
		p.report(ctx, key, pending)
	}()
}

// Report is the body of POST /v1/codesynapse/failover: counts and times
// only.
type Report struct {
	Windows []ReportWindow `json:"windows"`
}

// ReportWindow is one outage window as reported.
type ReportWindow struct {
	Start    time.Time      `json:"start"`
	End      time.Time      `json:"end"`
	Requests int            `json:"requests"`
	Models   map[string]int `json:"models,omitempty"`
}

func (p *Proxy) report(ctx context.Context, key string, pending []Window) {
	var rep Report
	for _, w := range pending {
		rep.Windows = append(rep.Windows, ReportWindow{Start: w.Start.UTC(), End: w.End.UTC(), Requests: w.Requests, Models: w.Models})
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.gateway.JoinPath(ReportPath).String(), bytes.NewReader(b))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(keyHeader, key)
	resp, err := p.gw.Do(req)
	if err != nil {
		p.log.Info("failover report not sent; will try again", "error", errClass(err))
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode/100 == 2:
		p.counts.drop(pending)
		p.log.Info("failover counts reported", "windows", len(pending))
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusNotFound,
		resp.StatusCode == http.StatusRequestEntityTooLarge:
		// This gateway cannot take them (an older API, or counts it
		// refuses); keeping them would retry forever.
		p.counts.drop(pending)
		p.log.Warn("the gateway did not accept the failover counts; dropped", "status", resp.StatusCode)
	default:
		p.log.Info("failover report refused for now; will try again", "status", resp.StatusCode)
	}
}

// Run serves on ln until ctx ends, the proxy has been idle for its idle
// time, or its config was removed or changed (mcp install --remove).
func (p *Proxy) Run(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			return p.shutdown(ctx, srv)
		case <-t.C:
			if why := p.stopReason(); why != "" {
				p.log.Info("gateway proxy stopping", "reason", why)
				return p.shutdown(ctx, srv)
			}
		}
	}
}

func (p *Proxy) stopReason() string {
	if c, err := p.files.Load(); err != nil || c != p.cfg {
		return "config removed or changed"
	}
	idle := time.Since(time.Unix(0, p.lastUse.Load()))
	if p.inflight.Load() == 0 && idle >= p.idle {
		return "idle"
	}
	return ""
}

// shutdown lets requests in flight finish, even when ctx has ended.
func (p *Proxy) shutdown(ctx context.Context, srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return srv.Close()
	}
	return nil
}

// Listen binds the proxy's port on 127.0.0.1 only.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	if err := ValidateListen(addr); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// ValidateListen refuses anything but a loopback IP: the proxy passes on a
// key and a model credential, so it must never be reachable from elsewhere.
func ValidateListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return errors.New("the gateway proxy listens on a loopback address only, such as 127.0.0.1")
	}
	return nil
}

// problem answers in the shape Anthropic clients parse.
func problem(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": msg},
	})
}
