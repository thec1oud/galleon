package process

import "golang.org/x/sys/unix"

// SO_REUSEPORT_LB is FreeBSD's load-balancing variant; plain SO_REUSEPORT
// there does not spread connections.
const soReusePort = unix.SO_REUSEPORT_LB
