// Package netguard stops an HTTP client from connecting to addresses that are never a legitimate destination for a URL a user chose: the machine it runs on and link-local services such as cloud metadata.
package netguard

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// ErrForbidden means the destination address is loopback, link-local, unspecified or multicast.
var ErrForbidden = errors.New("destination address not allowed")

// Control is a net.Dialer.Control that refuses forbidden addresses. It runs on the address the dialer resolved, after DNS and for every redirect hop, so a hostname cannot point past it. Private ranges stay allowed: in-cluster services live there, and pod egress policy governs them.
func Control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q is not an address", ErrForbidden, address)
	}
	// An IPv4-mapped IPv6 address is the IPv4 address.
	ip := ap.Addr().Unmap()
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("%w: %s", ErrForbidden, ip)
	}
	return nil
}

// Transport is a clone of http.DefaultTransport whose dialer applies Control.
func Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: Control}).DialContext
	return t
}
