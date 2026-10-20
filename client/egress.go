package client

import (
	"net"
	"net/netip"
	"syscall"

	"github.com/thec1oud/galleon/errs"
)

// guard refuses connections to addresses a server-side client has no business
// reaching by default: loopback, private, link-local (cloud metadata
// included), shared, reserved, unspecified and multicast addresses. It runs at
// dial time on the resolved address, so neither a DNS answer nor a redirect
// gets around it.
type guard struct {
	allow []netip.Prefix
	off   bool
}

// blocked are the ranges netip's predicates don't cover.
var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("100::/64"),        // discard
}

// nat64 is the well-known NAT64 prefix, whose addresses reach the IPv4
// address in their last four bytes.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

func newGuard(cfg Config) (*guard, error) {
	g := &guard{off: cfg.AllowPrivate}
	for _, n := range cfg.AllowNetworks {
		p, err := netip.ParsePrefix(n)
		if err != nil {
			return nil, err
		}
		g.allow = append(g.allow, p.Masked())
	}
	return g, nil
}

func (g *guard) control(_, address string, _ syscall.RawConn) error {
	if g.off {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return errs.New(errs.PermissionDenied, "galleon.egress_denied", "the client can't tell where "+address+" is")
	}
	if !g.permits(ip) {
		return errs.New(errs.PermissionDenied, "galleon.egress_denied", "the client may not connect to "+ip.String())
	}
	return nil
}

func (g *guard) permits(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	for _, p := range g.allow {
		if p.Contains(ip) {
			return true
		}
	}
	if nat64.Contains(ip) {
		b := ip.As16()
		return g.permits(netip.AddrFrom4([4]byte(b[12:])))
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
