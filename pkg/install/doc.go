// Package install implements the installation forms of C-54 that touch one
// cluster: installing into an existing cluster and bootstrapping a bare host,
// plus the uninstall and rescue paths that follow an installation.
//
// The install invariant of C-15 and the injection list of C-58 live in the
// install path itself: a run takes a snapshot before and after, reads the API
// server audit log between them and reports what it measured. An install whose
// audit cannot be read reports an unmeasured window instead of claiming the
// promise held (ADR-017).
//
// Migration: tsic404/kosadm takes over the installation, uninstall, rescue and
// the KOS self-upgrade path (C-55) in stage 2. kosd keeps the first-Owner claim
// window and the identity store the control plane serves (pkg/identity, C-63,
// C-68).
package install
