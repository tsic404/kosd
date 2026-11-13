package kosdtc155

import (
	"bytes"
	"os"
	"testing"

	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// The entries under testdata/sdk are the manifest and the single UI asset the
// author SDK (github.com/tsic404/kos-sdk packaging.Pack, commit 448f542) wrote
// for a system application; sdkDigest is the digest that packer published for
// them, so a change in how this kernel digests a package fails these cases.
const sdkDigest = "sha256:e25e74a844fa581e3e7948bd918828faf9dee126453efe12b91356e9275e8fd9"

const (
	sdkComponent = "kos-files-sdk"
	sdkExtension = "kos.files"
	sdkApp       = "files"
)

// TestTC155SDKPackedArtifactInstalls checks KOSD-C-17 against an artifact the
// author SDK packed: the install path derives the digest the SDK published, the
// declaration loads with its application, and the application becomes visible.
func TestTC155SDKPackedArtifactInstalls(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}

	archive := sdkPackage(t, nil)
	artifact, err := extension.Read(archive)
	if err != nil {
		t.Fatalf("read the SDK package: %v", err)
	}
	if artifact.Digest != sdkDigest {
		t.Fatalf("digest = %q, want the digest the SDK published %q", artifact.Digest, sdkDigest)
	}
	loaded, err := artifact.Declaration.Load()
	if err != nil {
		t.Fatalf("load the SDK declaration: %v", err)
	}
	if loaded.Identifier != sdkExtension || len(loaded.Contributions.Apps) != 1 ||
		loaded.Contributions.Apps[0].ID != sdkApp {
		t.Fatalf("declaration = %s apps = %+v, want the application the SDK packed",
			loaded.Identifier, loaded.Contributions.Apps)
	}

	installation := installArtifact(t, cluster, shell, sdkComponent, "test://"+sdkComponent, archive, sdkDigest, artifacts)
	result, found := installation.Component(sdkComponent)
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want ready", result, found)
	}
	if result.Digest != sdkDigest {
		t.Fatalf("verified digest = %q, want %q", result.Digest, sdkDigest)
	}
	apps := installation.Registry().Apps()
	if len(apps) != 1 || apps[0].Manifest.ID != sdkApp || apps[0].Component != sdkComponent {
		t.Fatalf("apps = %+v, want the SDK package's application", apps)
	}
	if apps[0].Manifest.Icon != "assets/icon.svg" {
		t.Fatalf("app icon = %q, want the package asset", apps[0].Manifest.Icon)
	}
}

// TestTC155SDKDigestCoversTheAsset checks KOSD-C-17: the digest the SDK
// published covers its UI asset, so an artifact whose asset changed under that
// digest installs nothing.
func TestTC155SDKDigestCoversTheAsset(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}

	const changedComponent = "kos-files-changed"
	changed := sdkPackage(t, []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>`))
	if _, err := extension.Read(changed); err == nil {
		t.Fatal("a package whose asset changed under the SDK digest was read")
	}
	installation := installArtifact(t, cluster, shell, changedComponent, "test://"+changedComponent, changed, sdkDigest, artifacts)
	result, found := installation.Component(changedComponent)
	if !found || result.Phase != extension.PhaseUnavailable || result.Reason != extension.ReasonDigestMismatch {
		t.Fatalf("component = %+v (found %v), want unavailable on a digest mismatch", result, found)
	}
	if apps := installation.Registry().Apps(); len(apps) != 0 {
		t.Fatalf("apps = %+v, want none from a refused package", apps)
	}
}

// sdkPackage rebuilds the archive from the SDK's entries. A replacement swaps
// the icon content while the SDK digest stays in the archive. The fixtures end
// with a newline the packed entries do not carry, so it is stripped here: the
// digest covers the entry bytes, not the file layout.
func sdkPackage(t *testing.T, replacement []byte) []byte {
	t.Helper()
	manifest := sdkEntry(t, "testdata/sdk/manifest.json")
	iconContent := sdkEntry(t, "testdata/sdk/assets/icon.svg")
	if replacement != nil {
		iconContent = replacement
	}
	return tarGz(t, [][2][]byte{
		{[]byte(extension.ManifestEntry), manifest},
		{[]byte("assets/icon.svg"), iconContent},
		{[]byte(extension.DigestEntry), []byte(sdkDigest + "\n")},
	})
}

// sdkEntry reads one fixture entry, dropping the file's trailing newline.
func sdkEntry(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read the SDK fixture %s: %v", name, err)
	}
	return bytes.TrimSuffix(content, []byte("\n"))
}
