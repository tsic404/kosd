// Command kos-control-plane is the deployable control-plane process: it serves
// the account and domain API browsers and extensions use (KOSD-C-12,
// KOSD-C-63), refreshes the contribution projection from the live components
// (KOSD-C-28) and runs every kernel reconcile subject in this one process
// (KOSD-C-03, KOSD-C-04, ADR-011).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tsic404/kosd/pkg/kernel"
)

func main() {
	if err := run(); err != nil {
		slog.Error("kos-control-plane stopped", "error", err)
		os.Exit(1)
	}
}

// run parses the flags, assembles the process and serves until it is asked to
// stop. The flag set is what the Deployment manifest passes, so the assembly
// under test is the assembly the cluster runs.
func run() error {
	options, err := kernel.ParseOptions(os.Args[1:])
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	process, err := kernel.Start(ctx, options)
	if err != nil {
		return err
	}
	defer process.Close()
	return process.Serve()
}
