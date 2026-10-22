package client

import "net/netip"

// Permits reports whether the egress guard lets ip through with allow open.
func Permits(ip netip.Addr, allow []string) bool {
	g, err := newGuard(Config{AllowNetworks: allow})
	if err != nil {
		panic(err)
	}
	return g.permits(ip)
}
