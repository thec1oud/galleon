package process

import (
	"context"
	"net"
)

// Listen opens a listener. Under a supervisor it sets SO_REUSEPORT, so every
// worker can bind the same address and the kernel spreads connections across
// them. Outside a supervisor it is a plain listen, so an accidental second
// process on the same port still fails loudly.
//
// Only Linux spreads connections across sockets sharing a port. On macOS and
// the BSDs other than FreeBSD the sockets can share it, but one of them gets
// the connections, so several workers there are for development only.
func Listen(ctx context.Context, network, address string) (net.Listener, error) {
	lc := net.ListenConfig{}
	if Supervised() && reusePortSupported {
		lc.Control = reusePort
	}
	return lc.Listen(ctx, network, address)
}
