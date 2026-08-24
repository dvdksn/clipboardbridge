//go:build unix

package clipboardbridge

import (
	"bytes"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionIDFromEnviron(t *testing.T) {
	env := func(entries ...string) []byte {
		var out []byte
		for _, e := range entries {
			out = append(out, []byte(e)...)
			out = append(out, 0)
		}
		return out
	}

	t.Run("finds the value", func(t *testing.T) {
		got, err := sessionIDFromEnviron(env("PATH=/usr/bin", hostSessionIDEnv+"=sess-abc", "TERM=xterm"))
		require.NoError(t, err)
		require.Equal(t, "sess-abc", got)
	})

	t.Run("requires the whole name to match", func(t *testing.T) {
		// A variable whose name merely ends with ours, and one whose value
		// happens to contain the name, must both be ignored.
		_, err := sessionIDFromEnviron(env(
			"X_"+hostSessionIDEnv+"=nope",
			"OTHER="+hostSessionIDEnv+"=nope",
		))
		require.ErrorContains(t, err, "not present")
	})

	for _, tc := range []struct {
		name string
		raw  []byte
		msg  string
	}{
		{name: "absent", raw: env("PATH=/usr/bin"), msg: "not present"},
		{name: "empty block", raw: nil, msg: "not present"},
		{name: "set but empty", raw: env(hostSessionIDEnv + "="), msg: "set but empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sessionIDFromEnviron(tc.raw)
			require.ErrorContains(t, err, tc.msg)
		})
	}

	t.Run("never puts the value in the error", func(t *testing.T) {
		// Errors here are logged, and the id is a capability.
		_, err := sessionIDFromEnviron(env(hostSessionIDEnv + "="))
		require.NotContains(t, err.Error(), "=")
	})
}

func TestReadSessionIDFromProcessRejects(t *testing.T) {
	self := uint32(os.Getuid())

	t.Run("a nonsense pid", func(t *testing.T) {
		_, err := readSessionIDFromProcess(0, self)
		require.ErrorContains(t, err, "invalid peer pid")
	})

	t.Run("a process owned by another user", func(t *testing.T) {
		_, err := readSessionIDFromProcess(int32(os.Getpid()), self+1)
		require.ErrorContains(t, err, "owned by uid")
	})
}

func TestSessionIDFromEnvironReaderRejectsOversize(t *testing.T) {
	// A block one byte past the cap is rejected rather than truncated: a cut
	// landing mid-value would otherwise produce a plausible-looking partial id.
	block := bytes.Repeat([]byte("PADDING=x\x00"), (maxEnvironBytes/10)+1)
	require.Greater(t, len(block), maxEnvironBytes)

	_, err := sessionIDFromEnvironReader(bytes.NewReader(block))
	require.ErrorContains(t, err, "exceeds")

	t.Run("and accepts a block at the cap", func(t *testing.T) {
		entry := []byte(hostSessionIDEnv + "=sess-ok\x00")
		padded := append(bytes.Repeat([]byte{0}, maxEnvironBytes-len(entry)), entry...)
		got, err := sessionIDFromEnvironReader(bytes.NewReader(padded))
		require.NoError(t, err)
		require.Equal(t, "sess-ok", got)
	})
}
