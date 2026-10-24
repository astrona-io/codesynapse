package gitx

import (
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Addresses ValidateRemote checked are handed to git, so it connects to those
// and to nothing a second lookup might return. The entry is a curl resolve
// override (http.curloptResolve, git 2.37 or newer; older git ignores the key
// and resolves by itself).
const (
	pinTTL     = 10 * time.Minute
	maxPinHost = 512
)

type pin struct {
	entry string
	until time.Time
}

var pins = struct {
	sync.Mutex
	m map[string]pin
}{m: map[string]pin{}}

func pinKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

// rememberPin records the addresses a validated https remote resolved to. A
// literal IP needs no override.
func rememberPin(u *url.URL, addrs []net.IPAddr) {
	if net.ParseIP(u.Hostname()) != nil || len(addrs) == 0 {
		return
	}
	var list []string
	for _, a := range addrs {
		ip := a.IP.String()
		if a.IP.To4() == nil {
			ip = "[" + ip + "]"
		}
		list = append(list, ip)
	}
	key := pinKey(u)
	now := time.Now()
	pins.Lock()
	defer pins.Unlock()
	if len(pins.m) >= maxPinHost {
		for k, p := range pins.m {
			if now.After(p.until) {
				delete(pins.m, k)
			}
		}
		if len(pins.m) >= maxPinHost { // still full: start over rather than grow
			clear(pins.m)
		}
	}
	pins.m[key] = pin{entry: key + ":" + strings.Join(list, ","), until: now.Add(pinTTL)}
}

// pinFor returns the resolve override for remote, or "" when its host was
// not validated recently (or is an IP address).
func pinFor(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	pins.Lock()
	defer pins.Unlock()
	p, ok := pins.m[pinKey(u)]
	if !ok || time.Now().After(p.until) {
		delete(pins.m, pinKey(u))
		return ""
	}
	return p.entry
}
