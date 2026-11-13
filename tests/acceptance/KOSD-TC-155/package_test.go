package kosdtc155

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// installArtifact creates one component over a package and runs an install pass.
func installArtifact(t *testing.T, cluster *harness.Cluster, shell *harness.Shell, name, reference string, content []byte, digest string, artifacts extension.MemoryArtifacts) *extension.Installation {
	t.Helper()
	artifacts[reference] = content
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(name, reference, digest)})
	installation := cluster.ExtensionState(t, artifacts)
	shell.Use(installation)
	return installation
}

// TestTC155DigestCoversTheManifestAndEveryAsset checks KOSD-C-17: a package
// whose asset changed while the named digest stayed behind is refused.
func TestTC155DigestCoversTheManifestAndEveryAsset(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}
	declaration := filemanager()

	contentA, digestA, err := extension.Pack(declaration, []extension.Asset{{
		Path: "assets/icon.svg", Content: []byte("<svg>A</svg>"),
	}})
	if err != nil {
		t.Fatalf("pack icon A: %v", err)
	}
	contentB, digestB, err := extension.Pack(declaration, []extension.Asset{{
		Path: "assets/icon.svg", Content: []byte("<svg>B</svg>"),
	}})
	if err != nil {
		t.Fatalf("pack icon B: %v", err)
	}
	if digestA == digestB {
		t.Fatal("changing an asset did not change the package digest")
	}
	if _, err := extension.Read(contentA); err != nil {
		t.Fatalf("read the package carrying icon A: %v", err)
	}

	installation := installArtifact(t, cluster, shell, "asset-mismatch", "test://icon-b", contentB, digestA, artifacts)
	result, _ := installation.Component("asset-mismatch")
	if result.Phase != extension.PhaseUnavailable || result.Reason != extension.ReasonDigestMismatch {
		t.Fatalf("component = %s/%s, want unavailable on a digest mismatch", result.Phase, result.Reason)
	}
	if apps := installation.Registry().Apps(); len(apps) != 0 {
		t.Fatalf("apps = %+v, want none from a refused package", apps)
	}

	changed := declaration
	changed.Version = "1.2.1"
	if _, changedDigest, err := extension.Pack(changed, []extension.Asset{{
		Path: "assets/icon.svg", Content: []byte("<svg>A</svg>"),
	}}); err != nil {
		t.Fatalf("pack changed declaration: %v", err)
	} else if changedDigest == digestA {
		t.Fatal("changing the declaration did not change the package digest")
	}
}

// TestTC155PackageCarriesOnlyItsDeclaredEntries checks that an entry outside
// the package structure refuses the package (KOSD-C-17).
func TestTC155PackageCarriesOnlyItsDeclaredEntries(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}
	manifest, err := json.Marshal(filemanager())
	if err != nil {
		t.Fatalf("encode declaration: %v", err)
	}
	content, digest := packageArchive(t, manifest,
		map[string][]byte{"assets/icon.svg": []byte("<svg/>")},
		map[string][]byte{"notes.txt": []byte("extra")})
	installation := installArtifact(t, cluster, shell, "open", "test://open", content, digest, artifacts)
	result, _ := installation.Component("open")
	if result.Phase != extension.PhaseUnavailable {
		t.Fatalf("component = %+v, want unavailable for an undeclared entry", result)
	}
	if apps := installation.Registry().Apps(); len(apps) != 0 {
		t.Fatalf("apps = %+v, want none from a refused package", apps)
	}
}

// TestTC155AppManifestRequiresEveryField checks KOSD-C-73: an App declaration
// missing a required field makes the declaration invalid.
func TestTC155AppManifestRequiresEveryField(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}
	document, err := json.Marshal(filemanager())
	if err != nil {
		t.Fatalf("encode declaration: %v", err)
	}
	decoded := decodeObject(t, document)
	app := decoded["contributions"].(map[string]any)["apps"].([]any)[0].(map[string]any)
	delete(app, "permissions")
	manifest, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode declaration: %v", err)
	}
	content, digest := packageArchive(t, manifest, map[string][]byte{"assets/icon.svg": []byte("<svg/>")}, nil)
	installation := installArtifact(t, cluster, shell, "missing-field", "test://missing-field", content, digest, artifacts)
	result, _ := installation.Component("missing-field")
	if result.Phase != extension.PhaseUnavailable || result.Reason != extension.ReasonInvalidDeclaration {
		t.Fatalf("component = %s/%s, want an invalid declaration", result.Phase, result.Reason)
	}
	if !strings.Contains(result.Message, "permissions") {
		t.Fatalf("message = %q, want the missing field named", result.Message)
	}
	if apps := installation.Registry().Apps(); len(apps) != 0 {
		t.Fatalf("apps = %+v, want none from an invalid declaration", apps)
	}
}

// TestTC155InvalidAppClassDoesNotTakeOtherClassesDown checks C-28: an
// out-of-set permission drops the application while the page still loads.
func TestTC155InvalidAppClassDoesNotTakeOtherClassesDown(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageAssetsWith(t, fileComponent, filemanager(), icon(), artifacts, shell.Retention())
	shell.Use(installation)

	broken := filemanager()
	broken.Identifier = "acme.broken"
	broken.Contributions.Apps[0].ID = "broken"
	broken.Contributions.Apps[0].Permissions = []string{"camera"}
	broken.Contributions.Pages = notes().Contributions.Pages
	installation = cluster.InstallPackageAssetsWith(t, "broken", broken, icon(), artifacts, shell.Retention())
	shell.Use(installation)

	result, found := installation.Component("broken")
	if !found || result.Phase != extension.PhaseDegraded {
		t.Fatalf("broken component = %+v (found %v), want degraded", result, found)
	}
	apps := installation.Registry().Apps()
	if len(apps) != 1 || apps[0].Manifest.ID != fileApp {
		t.Fatalf("apps = %+v, want only the valid application", apps)
	}
	if pages := installation.Registry().Pages(); len(pages) != 1 || pages[0].ID != "notes" {
		t.Fatalf("pages = %+v, want the page of the class that still loaded", pages)
	}
}

// packageArchive builds one package from a manifest and asset set; extra
// entries are written outside the structure, with the digest still computed
// over the declared content only.
func packageArchive(t *testing.T, manifest []byte, assets, extra map[string][]byte) ([]byte, string) {
	t.Helper()
	names := make([]string, 0, len(assets))
	for name := range assets {
		names = append(names, name)
	}
	sort.Strings(names)
	ordered := make([]extension.Asset, 0, len(names))
	for _, name := range names {
		ordered = append(ordered, extension.Asset{Path: name, Content: assets[name]})
	}
	digest := extension.DigestOf(manifest, ordered)

	entries := [][2][]byte{{[]byte(extension.ManifestEntry), manifest}}
	for _, name := range names {
		entries = append(entries, [2][]byte{[]byte(name), assets[name]})
	}
	extraNames := make([]string, 0, len(extra))
	for name := range extra {
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		entries = append(entries, [2][]byte{[]byte(name), extra[name]})
	}
	entries = append(entries, [2][]byte{[]byte(extension.DigestEntry), []byte(digest + "\n")})
	return tarGz(t, entries), digest
}

// decodeObject decodes one JSON object.
func decodeObject(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	object := map[string]any{}
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatalf("decode object: %v", err)
	}
	return object
}

// tarGz writes one archive from name and content pairs.
func tarGz(t *testing.T, entries [][2][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for _, entry := range entries {
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     string(entry[0]),
			Mode:     0o644,
			Size:     int64(len(entry[1])),
			ModTime:  time.Unix(0, 0),
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatalf("write archive header: %v", err)
		}
		if _, err := archive.Write(entry[1]); err != nil {
			t.Fatalf("write archive entry: %v", err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatalf("close compression: %v", err)
	}
	return buffer.Bytes()
}
