//go:build unix

package clipboardbridge

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
)

// Run listens on the sandbox's Wayland socket and serves the data-control
// clipboard protocol until ctx is cancelled. The socket location mirrors how a
// Wayland client (arboard/wl-clipboard-rs) resolves it: an absolute
// WAYLAND_DISPLAY is used verbatim, otherwise it is joined onto
// XDG_RUNTIME_DIR. Both must match the env the agent sees.
func Run(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	sockPath := socketPath()
	if dir := filepath.Dir(sockPath); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create runtime dir %s: %w", dir, err)
		}
	}
	// Remove a stale socket from a previous run; ListenUnix won't bind over it.
	_ = os.Remove(sockPath)

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sockPath, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on %s: %w", sockPath, err)
	}
	defer l.Close()
	_ = os.Chmod(sockPath, 0o700)
	log.Info("clipboard-bridge: listening", "socket", sockPath)

	srv := newServer(newProxyClipboard(), log)

	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	for {
		uc, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil // shutting down
			}
			return fmt.Errorf("accept: %w", err)
		}
		go srv.serve(ctx, uc)
	}
}

// socketPath resolves the Wayland socket path the same way wayland-client does.
func socketPath() string {
	wl := os.Getenv("WAYLAND_DISPLAY")
	if wl == "" {
		wl = "wayland-0"
	}
	if filepath.IsAbs(wl) {
		return wl
	}
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		// Conventional per-user runtime dir for uid 1000 (the sandbox agent).
		runtimeDir = "/run/user/1000"
	}
	return filepath.Join(runtimeDir, wl)
}
