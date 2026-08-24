//go:build linux

package clipboardbridge

import (
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// helperSocketEnv names the socket the child half of TestSessionIDForPeer
// should dial. Its presence is what marks a re-executed test binary as the
// child rather than an ordinary run.
const helperSocketEnv = "CLIPBOARD_BRIDGE_TEST_HELPER_SOCKET"

// TestSessionIDForPeerConnect is the child half of TestSessionIDForPeer: a real
// separate process, so the parent reads a real peer credential and a real
// /proc/<pid>/environ rather than its own.
func TestSessionIDForPeerConnect(t *testing.T) {
	sock := os.Getenv(helperSocketEnv)
	if sock == "" {
		t.Skip("child half of TestSessionIDForPeer; runs only when re-executed")
	}
	conn, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer conn.Close()
	// Stay alive and connected until the parent closes us, so it can read our
	// credentials while we demonstrably exist.
	_, _ = io.Copy(io.Discard, conn)
}

// startPeer re-executes this test binary so it connects to sock, with
// sessionID in its environment (omitted entirely when empty), and returns the
// accepted server side of that connection.
func startPeer(t *testing.T, sessionID string) *net.UnixConn {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "wayland-test")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })

	// Drop any inherited copy of these before setting our own: the parser takes
	// the first match, and this process may itself be running in a sandbox.
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, hostSessionIDEnv+"=") || strings.HasPrefix(e, helperSocketEnv+"=") {
			continue
		}
		env = append(env, e)
	}
	env = append(env, helperSocketEnv+"="+sock)
	if sessionID != "" {
		env = append(env, hostSessionIDEnv+"="+sessionID)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSessionIDForPeerConnect$", "-test.timeout=60s")
	cmd.Env = env
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	require.NoError(t, l.SetDeadline(time.Now().Add(30*time.Second)))
	uc, err := l.AcceptUnix()
	require.NoError(t, err)
	t.Cleanup(func() { _ = uc.Close() })
	return uc
}

func TestSessionIDForPeer(t *testing.T) {
	t.Run("reads the session id out of the connecting process", func(t *testing.T) {
		got, err := sessionIDForPeer(startPeer(t, "sess-from-peer-process"))
		require.NoError(t, err)
		require.Equal(t, "sess-from-peer-process", got)
	})

	t.Run("reports no session when the client has none", func(t *testing.T) {
		got, err := sessionIDForPeer(startPeer(t, ""))
		require.ErrorContains(t, err, "not present")
		require.Empty(t, got, "a client without a session must not yield one")
	})
}
