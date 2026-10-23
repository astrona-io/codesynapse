package app

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParsePrefixes reads "10.0.0.0/8,192.168.1.5" (single addresses allowed),
// the format of CODESYNAPSE_TRUSTED_PROXIES.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "/") {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, err
			}
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ClientAddr is the address a request came from. X-Forwarded-For counts
// only when the connection is from a trusted proxy, and then only its last
// hop (the address that proxy saw), so clients cannot choose their own.
func ClientAddr(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !trustedProxy(ip.Unmap(), trusted) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	if last, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1])); err == nil {
		return last.Unmap().String()
	}
	return host
}

func trustedProxy(ip netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
