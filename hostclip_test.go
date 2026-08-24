//go:build unix

package clipboardbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProxyClipboardRequestBody pins the request the bridge sends against each
// environment the sandbox can hand it: the session resolved from the client,
// the bridge's own SBX_HOST_SESSION_ID as a fallback, the older SBX_HOST_*
// values as host_env, and none of them when there is no session to be had.
func TestProxyClipboardRequestBody(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sessionID string
		env       map[string]string
		want      map[string]any
	}{
		{
			name:      "prefers the session resolved from the client",
			sessionID: "sess-from-peer",
			env:       map[string]string{hostSessionIDEnv: "sess-from-own-env"},
			want:      map[string]any{"type": clipboardImageType, "session_id": "sess-from-peer"},
		},
		{
			name: "falls back to the bridge's own session id",
			env:  map[string]string{hostSessionIDEnv: "sess-token-123"},
			want: map[string]any{"type": clipboardImageType, "session_id": "sess-token-123"},
		},
		{
			name: "falls back to host_env for a sandbox that names the session itself",
			env: map[string]string{
				"SBX_HOST_DISPLAY":         ":0",
				"SBX_HOST_WAYLAND_DISPLAY": "wayland-0",
			},
			want: map[string]any{
				"type":     clipboardImageType,
				"host_env": map[string]any{"DISPLAY": ":0", "WAYLAND_DISPLAY": "wayland-0"},
			},
		},
		{
			name: "sends neither when not started from an attach",
			want: map[string]any{"type": clipboardImageType},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(raw, &got))
			}))
			defer srv.Close()
			t.Setenv(proxyURLEnv, srv.URL)
			// Clear both schemes so each case starts from a known environment
			// and an exact match on the body means what it says.
			t.Setenv(hostSessionIDEnv, "")
			for _, envKey := range hostEnvKeys {
				t.Setenv(envKey, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := newProxyClipboard().imagePNG(context.Background(), tc.sessionID)

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
