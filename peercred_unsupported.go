//go:build unix && !linux

package clipboardbridge

import (
	"errors"
	"net"
)

// peerCredential is Linux-only. SO_PEERCRED has no portable equivalent, and the
// session lookup it feeds reads /proc, which is Linux-specific as well. The
// bridge still builds and runs elsewhere: session resolution fails here, and
// the caller falls back to the bridge's own environment.
func peerCredential(*net.UnixConn) (pid int32, uid uint32, err error) {
	return 0, 0, errors.New("peer credentials are not available on this platform")
}
