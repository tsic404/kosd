// Package kernel assembles the deployable control-plane process: one process
// serves the read projection and runs every kernel reconcile subject
// (KOSD-C-03, KOSD-C-04, ADR-011). The deployment manifest starts this process;
// nothing else writes these objects.
package kernel

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/tsic404/kosd/pkg/install"
)

// Defaults a deployment may override by flag.
const (
	defaultAddress = ":8080"
	// DefaultDatabasePath is where the control-plane database lives. It is the
	// identity store the process owns, not a kernel object.
	DefaultDatabasePath = "/var/lib/kosd/kos.db"
	// DefaultBackupRoot is where the backup worker reaches the volume data it
	// copies (C-42).
	DefaultBackupRoot = "/var/lib/kosd/backup"

	// memorySampleInterval keeps the degradation protection measurable
	// (KOSD-C-57). It runs well inside the five-minute window, so one missed
	// read cannot turn the health probe unavailable.
	memorySampleInterval = 10 * time.Second
	// extensionInstallInterval bounds how often the projection is refreshed
	// from the live components (KOSD-C-28).
	extensionInstallInterval = 10 * time.Second
	// installPassTimeout is the hard limit of one install pass.
	installPassTimeout = 30 * time.Second
	// catalogueSyncInterval bounds how often an installable entry's definition
	// is written (KOSD-C-24, KOSD-C-03).
	catalogueSyncInterval = 10 * time.Second
	// sweepInterval is how often the process lists the kernel objects again.
	// The kernel has no watch source yet, so a sweep is what hands a new object
	// to its reconciler; a settled object keeps its own bounded requeue
	// (KOSD-C-06).
	sweepInterval = 10 * time.Second
	// shutdownTimeout bounds the graceful drain after a signal.
	shutdownTimeout = 30 * time.Second
)

// Options is the deployment configuration one control-plane process starts
// with. The flags mirror the arguments the Deployment manifest passes; Database
// and Packages name paths an operator may relocate.
type Options struct {
	// Address is the HTTP listen address.
	Address string
	// Kubeconfig is the path to a kubeconfig; empty uses the in-cluster
	// configuration.
	Kubeconfig string
	// DatabasePath is the control-plane database.
	DatabasePath string
	// PackagesPath is the directory extension packages are read from.
	PackagesPath string
	// MemoryLimit is the degradation threshold in bytes; zero selects the
	// default.
	MemoryLimit int64
	// TerminalExecutor names the terminal executor; empty leaves sessions
	// unavailable, kubernetes executes Pod sessions.
	TerminalExecutor string
	// TerminalNodeIsolated asserts that root@node cannot read control-plane
	// credentials; without it node sessions are refused.
	TerminalNodeIsolated bool
	// BackupRoot is the directory the backup worker reaches volume data under.
	BackupRoot string
	// BackupImage is the digest-pinned backup component image a schedule job
	// runs; a policy is blocked without one (KOSD-C-17, KOSD-C-41).
	BackupImage string
	// MigrationImage is the image that runs one migration step (KOSD-C-40).
	MigrationImage string
	// ControllerSource resolves the implementation this process hosts for one
	// declared extension controller (KOSD-C-20). This binary links none: a
	// declared controller stays unavailable until a first-party backend that
	// embeds this process supplies one, and its objects project Unknown
	// (KOSD-C-08). It is not a flag: an implementation is code, not
	// configuration.
	ControllerSource ControllerSource
}

// ParseOptions reads the process arguments. The deployment manifest's container
// args are the same arguments, so a case can parse the manifest and start the
// process exactly as the cluster does.
func ParseOptions(args []string) (Options, error) {
	options := Options{}
	flags := flag.NewFlagSet("kos-control-plane", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.Address, "address", defaultAddress, "HTTP listen address")
	flags.StringVar(&options.Kubeconfig, "kubeconfig", "", "path to the kubeconfig; empty uses the in-cluster configuration")
	flags.StringVar(&options.DatabasePath, "database", DefaultDatabasePath, "path to the control-plane database")
	flags.StringVar(&options.PackagesPath, "packages", install.PackageDirectory, "directory extension packages are read from")
	flags.Int64Var(&options.MemoryLimit, "memory-limit", 0, "degradation threshold in bytes; zero selects the default")
	flags.StringVar(&options.TerminalExecutor, "terminal-executor", "", "terminal executor: empty leaves sessions unavailable, kubernetes executes Pod sessions")
	flags.BoolVar(&options.TerminalNodeIsolated, "terminal-node-isolated", false, "assert that root@node cannot read control-plane credentials; without it node sessions are refused")
	flags.StringVar(&options.BackupRoot, "backup-root", DefaultBackupRoot, "directory the backup worker reaches volume data under")
	flags.StringVar(&options.BackupImage, "backup-image", "", "digest-pinned backup component image a schedule job runs")
	flags.StringVar(&options.MigrationImage, "migration-image", "", "image that runs one migration step")
	if err := flags.Parse(args); err != nil {
		return Options{}, fmt.Errorf("parse arguments: %w", err)
	}
	return options, nil
}
