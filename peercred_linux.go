//go:build linux

package clipboardbridge

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredential returns the pid and uid of the process on the other end of uc.
// The kernel records these at connect(2) time and the client cannot forge them,
// which is what makes them usable to decide whose environment to read.
func peerCredential(uc *net.UnixConn) (pid int32, uid uint32, err error) {
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, fmt.Errorf("syscall conn: %w", err)
	}
	var (
		cred    *unix.Ucred
		credErr error
	)
	// Control runs the closure with the descriptor held open; the getsockopt
	// result has to come back through captured variables rather than a return.
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, 0, fmt.Errorf("control socket: %w", err)
	}
	if credErr != nil {
		return 0, 0, fmt.Errorf("getsockopt SO_PEERCRED: %w", credErr)
	}
	return cred.Pid, cred.Uid, nil
}
