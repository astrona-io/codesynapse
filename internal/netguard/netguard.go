// Package netguard refuses connections to addresses that are not on the
// public internet: loopback, private, link-local (which holds the cloud
// metadata service at 169.254.169.254), carrier-grade NAT, multicast and
// unspecified ones. HTTP clients whose host comes from a user or an agent
// (identity providers, forge APIs, fetched web pages) set Control on their
// dialer, so the check runs on the address actually connected to and a DNS
// answer that changes after an earlier check cannot get past it.
package netguard

import (
	"net"
	"net/netip"
	"syscall"
)

// sharedSpace is RFC 6598 carrier-grade NAT space: not routed on the
// internet, used inside providers (some put their metadata service there)
// and by overlay networks.
var sharedSpace = netip.MustParsePrefix("100.64.0.0/10")

// thisNetwork is 0.0.0.0/8, which some systems route to the local host.
var thisNetwork = netip.MustParsePrefix("0.0.0.0/8")

// Internal reports whether ip is an address user-chosen hosts may not reach.
func Internal(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if a, ok := netip.AddrFromSlice(ip); ok {
		a = a.Unmap()
		return sharedSpace.Contains(a) || thisNetwork.Contains(a)
	}
	return false
}

// Control returns a net.Dialer Control function that answers refused for
// every internal address (and anything that is not an IP address).
func Control(refused error) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || Internal(ip) {
			return refused
		}
		return nil
	}
}
