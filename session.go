//go:build unix

package clipboardbridge

// session resolves which sandbox attach a paste belongs to.
//
// The bridge is started once per sandbox, before any attach exists, so it never
// has SBX_HOST_SESSION_ID in its own environment. The client asking for the
// paste does: a native clipboard reader (arboard/wl-clipboard-rs) runs inside
// the attached agent session and inherits it. So we take the session from
// whoever opened the connection rather than from ourselves, which also means
// concurrent attaches each resolve to their own session instead of sharing one.

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// maxEnvironBytes caps how much of a client's environment block we read. The
// kernel already bounds a process's environment well below this; the cap is
// here so a corrupt or hostile /proc entry cannot make the bridge allocate
// without limit.
const maxEnvironBytes = 1 << 20

// sessionIDForPeer returns the sandbox session ID of the process on the other
// end of uc. An error means no session could be established for this client;
// callers treat that as "no session" rather than substituting another one.
func sessionIDForPeer(uc *net.UnixConn) (string, error) {
	pid, uid, err := peerCredential(uc)
	if err != nil {
		return "", err
	}
	// Only a peer running as this same user is one whose environment we are
	// entitled to read, and in a sandbox the agent shares our uid.
	if self := uint32(os.Getuid()); uid != self {
		return "", fmt.Errorf("peer uid %d is not the bridge uid %d", uid, self)
	}
	return readSessionIDFromProcess(pid, uid)
}

// readSessionIDFromProcess reads SBX_HOST_SESSION_ID from the environment of
// pid, which must be owned by expectedUID.
//
// /proc/<pid> is opened as a directory and environ read relative to that
// descriptor. A pid recycled between the two steps then yields a stale
// descriptor rather than a different process's environment, and the ownership
// check goes through the same descriptor so it cannot be raced either.
func readSessionIDFromProcess(pid int32, expectedUID uint32) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid peer pid %d", pid)
	}
	dirFD, err := unix.Open(fmt.Sprintf("/proc/%d", pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open /proc/%d: %w", pid, err)
	}
	defer unix.Close(dirFD)

	var st unix.Stat_t
	if err := unix.Fstat(dirFD, &st); err != nil {
		return "", fmt.Errorf("stat /proc/%d: %w", pid, err)
	}
	if st.Uid != expectedUID {
		return "", fmt.Errorf("/proc/%d is owned by uid %d, not %d", pid, st.Uid, expectedUID)
	}

	envFD, err := unix.Openat(dirFD, "environ", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open /proc/%d/environ: %w", pid, err)
	}
	f := os.NewFile(uintptr(envFD), "environ")
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxEnvironBytes))
	if err != nil {
		return "", fmt.Errorf("read /proc/%d/environ: %w", pid, err)
	}
	return sessionIDFromEnviron(raw)
}

// sessionIDFromEnviron extracts SBX_HOST_SESSION_ID from a NUL-delimited
// environment block. Entries are matched on the whole "NAME=" prefix, so a
// variable whose name merely ends with the same text cannot satisfy the lookup.
// The value is never included in the errors here: it is a capability, and these
// errors are logged.
func sessionIDFromEnviron(raw []byte) (string, error) {
	prefix := []byte(hostSessionIDEnv + "=")
	for entry := range bytes.SplitSeq(raw, []byte{0}) {
		value, found := bytes.CutPrefix(entry, prefix)
		if !found {
			continue
		}
		if len(value) == 0 {
			return "", fmt.Errorf("%s is set but empty", hostSessionIDEnv)
		}
		return string(value), nil
	}
	return "", fmt.Errorf("%s is not present in the peer environment", hostSessionIDEnv)
}
