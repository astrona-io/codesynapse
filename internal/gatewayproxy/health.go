package gatewayproxy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

// HealthPath answers a challenge with an HMAC of it under the per-user
// secret, so `--ensure` can tell our proxy from another program on the port
// without the secret ever crossing the socket.
const HealthPath = "/_codesynapse/gateway-proxy/health"

var nonceRe = regexp.MustCompile(`^[0-9a-f]{32,64}$`)

func proof(secret []byte, nonce string) string {
	m := hmac.New(sha256.New, secret)
	_, _ = io.WriteString(m, "codesynapse-gateway-proxy:"+nonce)
	return hex.EncodeToString(m.Sum(nil))
}

func (p *Proxy) health(w http.ResponseWriter, r *http.Request) {
	nonce := r.URL.Query().Get("nonce")
	if r.Method != http.MethodGet || !nonceRe.MatchString(nonce) {
		http.Error(w, "bad health check", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, proof(p.secret, nonce))
}

// PortState is what holds the proxy's port.
type PortState int

const (
	// PortFree means nothing listens there.
	PortFree PortState = iota
	// PortOurs means our proxy answers, holding this user's secret.
	PortOurs
	// PortStranger means something else answers, or does not answer as ours.
	PortStranger
)

func (s PortState) String() string {
	switch s {
	case PortFree:
		return "free"
	case PortOurs:
		return "ours"
	}
	return "taken by another program"
}

// healthTimeout keeps --ensure fast: the proxy answers from memory.
const healthTimeout = 800 * time.Millisecond

// Check says whether our proxy listens on 127.0.0.1:port.
func Check(ctx context.Context, port int, secret []byte) PortState {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	b := make([]byte, 16)
	_, _ = rand.Read(b) //nolint:errcheck // crypto/rand does not fail on supported platforms
	nonce := hex.EncodeToString(b)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+HealthPath+"?nonce="+nonce, nil)
	if err != nil {
		return PortStranger
	}
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return PortFree
		}
		return PortStranger
	}
	defer resp.Body.Close() //nolint:errcheck // read side
	got, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode == http.StatusOK && hmac.Equal(got, []byte(proof(secret, nonce))) {
		return PortOurs
	}
	return PortStranger
}
