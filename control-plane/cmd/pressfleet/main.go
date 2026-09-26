// Command pressfleet is the PressFleet control plane: API server, background
// workers, metrics collector, and embedded dashboard, plus the operator CLI
// (init, doctor).
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Options{Version: version, Stdout: os.Stdout, Stderr: os.Stderr})
	stop()
	os.Exit(code)
}
