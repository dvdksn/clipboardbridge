//go:build unix

// Command clipboard-bridge runs inside a Docker sandbox and serves the host
// clipboard to coding agents over a minimal Wayland data-control socket. Agents
// that read clipboard images via a native library (arboard / wl-clipboard-rs,
// e.g. Codex) connect to WAYLAND_DISPLAY and paste host screenshots with
// Ctrl+V; this process answers those reads by fetching the image from the
// sandbox proxy's /_sbx/clipboard endpoint. See the clipboardbridge package.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dvdksn/clipboardbridge"
)

// version is overridden at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	// run() owns the signal context so its deferred cancel runs before os.Exit.
	if err := run(log); err != nil {
		log.Error("clipboard-bridge: exited with error", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return clipboardbridge.Run(ctx, log)
}

// logLevel honors a CLIPBOARD_BRIDGE_DEBUG=1 toggle; otherwise warns and above
// so the bridge is quiet in normal sandbox logs.
func logLevel() slog.Level {
	if os.Getenv("CLIPBOARD_BRIDGE_DEBUG") == "1" {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}
