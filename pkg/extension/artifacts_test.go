package extension

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDirectoryArtifactsServesTheNamedPackage checks the directory source
// returns the file a component names, so the install path verifies real bytes
// (KOSD-C-17).
func TestDirectoryArtifactsServesTheNamedPackage(t *testing.T) {
	root := t.TempDir()
	content := []byte("package bytes")
	writeArtifactFile(t, root, "dist/kos-taskmanager-0.1.0.tar.gz", content)

	got, err := (DirectoryArtifacts{Root: root}).Fetch(context.Background(), "dist/kos-taskmanager-0.1.0.tar.gz")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("fetch = %q, want %q", got, content)
	}
}

// TestDirectoryArtifactsVerifiesTheBytesOnDisk checks the digest is compared
// against the content read from disk: a changed file under the named digest is
// refused, it is not reported as unavailable (KOSD-C-17).
func TestDirectoryArtifactsVerifiesTheBytesOnDisk(t *testing.T) {
	root := t.TempDir()
	content, digest, err := Pack(Declaration{Identifier: "kos.demo", Version: "1.0.0"}, nil)
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	writeArtifactFile(t, root, "kos-demo.tar.gz", content)
	source := DirectoryArtifacts{Root: root}

	fetched, err := source.Fetch(context.Background(), "kos-demo.tar.gz")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := (Package{Reference: "kos-demo.tar.gz", Digest: digest, Content: fetched}).Verify(); err != nil {
		t.Fatalf("verify the artifact on disk: %v", err)
	}

	changed, otherDigest, err := Pack(Declaration{Identifier: "kos.demo", Version: "1.1.0"}, nil)
	if err != nil {
		t.Fatalf("pack the changed declaration: %v", err)
	}
	if otherDigest == digest {
		t.Fatal("a changed declaration kept the digest")
	}
	writeArtifactFile(t, root, "kos-demo.tar.gz", changed)
	fetched, err = source.Fetch(context.Background(), "kos-demo.tar.gz")
	if err != nil {
		t.Fatalf("fetch the changed file: %v", err)
	}
	if _, err := (Package{Reference: "kos-demo.tar.gz", Digest: digest, Content: fetched}).Verify(); err == nil {
		t.Fatal("the changed file verified under the old digest")
	} else if !strings.Contains(err.Error(), otherDigest) {
		t.Fatalf("mismatch error = %q, want the digest the file carries", err)
	}
}

// TestDirectoryArtifactsRefusesReferencesOutsideItsRoot checks a component can
// only name a package the directory holds.
func TestDirectoryArtifactsRefusesReferencesOutsideItsRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.tar.gz")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatalf("write the outside file: %v", err)
	}
	for _, reference := range []string{"../outside.tar.gz", outside, "dist/../../outside.tar.gz", "", ".", ".."} {
		if _, err := (DirectoryArtifacts{Root: root}).Fetch(context.Background(), reference); err == nil {
			t.Fatalf("reference %q was served", reference)
		} else if !strings.Contains(err.Error(), "not a path inside the package directory") {
			t.Fatalf("reference %q error = %q, want the containment refusal", reference, err)
		}
	}
}

// TestDirectoryArtifactsReportsUnreadableArtifacts checks the fetch failure
// names what is missing and refuses a directory where a file is expected.
func TestDirectoryArtifactsReportsUnreadableArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a-directory"), 0o755); err != nil {
		t.Fatalf("make the directory: %v", err)
	}
	if _, err := (DirectoryArtifacts{Root: root}).Fetch(context.Background(), "absent.tar.gz"); err == nil {
		t.Fatal("a missing artifact was served")
	} else if !strings.Contains(err.Error(), "is not available under") || !strings.Contains(err.Error(), root) {
		t.Fatalf("missing artifact error = %q, want the reference and the directory", err)
	}
	if _, err := (DirectoryArtifacts{Root: root}).Fetch(context.Background(), "a-directory"); err == nil {
		t.Fatal("a directory was served as a package")
	}
	if _, err := (DirectoryArtifacts{}).Fetch(context.Background(), "a.tar.gz"); err == nil {
		t.Fatal("an unconfigured source served an artifact")
	}
}

// writeArtifactFile writes one package entry below root.
func writeArtifactFile(t *testing.T, root, reference string, content []byte) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(reference))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("make the package directory: %v", err)
	}
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatalf("write the package file: %v", err)
	}
}
