//go:build unix

package clipboardbridge

// hostclip fetches the host clipboard image over an HTTP endpoint (by default
// the Docker Sandboxes proxy; see defaultProxyClipboardURL). This is how the
// in-sandbox Wayland bridge reaches the host clipboard: arboard/wl-clipboard
// asks us (over the Wayland socket) for image/png, and we in turn fetch the
// bytes over HTTP.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// clipboardImageType is the only MIME type bridged. Text already crosses into
// the sandbox via the terminal's own paste over the PTY; only images need this.
const clipboardImageType = "image/png"

// defaultProxyClipboardURL is the Docker Sandboxes proxy's non-proxied clipboard
// endpoint, reachable from inside the sandbox at the gateway host. We deliberately
// bypass any HTTP(S)_PROXY env so the request hits this endpoint directly rather
// than being forwarded upstream.
const defaultProxyClipboardURL = "http://gateway.docker.internal:3128/_sbx/clipboard"

// proxyURLEnv overrides defaultProxyClipboardURL, letting the bridge target a
// clipboard endpoint other than the Docker Sandboxes default.
const proxyURLEnv = "CLIPBOARD_BRIDGE_PROXY_URL"

// hostSessionIDEnv is the opaque per-attach session ID the sandbox injects into
// an interactive session's environment. We relay it as session_id so the host
// resolves which graphical session to read from. Empty when the bridge was not
// started from an attach.
const hostSessionIDEnv = "SBX_HOST_SESSION_ID"

// hostEnvKeys map each graphical-session variable to the SBX_HOST_* environment
// variable that carries its host value into the sandbox. Superseded by
// hostSessionIDEnv and sent alongside it, so one binary serves an endpoint on
// either scheme; the sandbox sets one or the other, never both, so only one is
// ever populated. Removable once nothing reads host_env.
var hostEnvKeys = map[string]string{
	"DISPLAY":                  "SBX_HOST_DISPLAY",
	"XAUTHORITY":               "SBX_HOST_XAUTHORITY",
	"WAYLAND_DISPLAY":          "SBX_HOST_WAYLAND_DISPLAY",
	"XDG_RUNTIME_DIR":          "SBX_HOST_XDG_RUNTIME_DIR",
	"DBUS_SESSION_BUS_ADDRESS": "SBX_HOST_DBUS_SESSION_BUS_ADDRESS",
}

// hostClipboard reads the host clipboard via the proxy. It is an interface so
// the server can be tested without a live proxy.
type hostClipboard interface {
	// imagePNG returns the host clipboard image as PNG bytes, or an empty
	// slice when the host clipboard holds no image. A nil error with empty
	// bytes means "no image", not a failure.
	imagePNG(ctx context.Context) ([]byte, error)
}

// proxyClipboard is the production hostClipboard backed by the sandbox proxy.
type proxyClipboard struct {
	url    string
	client *http.Client
}

func newProxyClipboard() *proxyClipboard {
	url := defaultProxyClipboardURL
	if v := os.Getenv(proxyURLEnv); v != "" {
		url = v
	}
	return &proxyClipboard{
		url: url,
		client: &http.Client{
			// Bypass proxy env so the request reaches the endpoint directly.
			Transport: &http.Transport{Proxy: nil},
			Timeout:   10 * time.Second,
		},
	}
}

func (p *proxyClipboard) imagePNG(ctx context.Context) ([]byte, error) {
	hostEnv := map[string]string{}
	for hostKey, envKey := range hostEnvKeys {
		if v := os.Getenv(envKey); v != "" {
			hostEnv[hostKey] = v
		}
	}
	reqBody, err := json.Marshal(struct {
		Type      string            `json:"type"`
		SessionID string            `json:"session_id,omitempty"`
		HostEnv   map[string]string `json:"host_env,omitempty"`
	}{Type: clipboardImageType, SessionID: os.Getenv(hostSessionIDEnv), HostEnv: hostEnv})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clipboard request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 501 (endpoint disabled) or any error → treat as no image so paste
		// degrades to an empty clipboard rather than surfacing an error.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	// Cap the read so a misbehaving host can't stream unbounded bytes into the
	// sandbox. 64 MiB is far above any realistic screenshot.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read clipboard body: %w", err)
	}
	return data, nil
}
