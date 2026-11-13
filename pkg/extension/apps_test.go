package extension

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// validApp is a window application declaration of KOSD-C-73.
func validApp() App {
	return App{
		ID:      "files",
		Name:    "Files",
		Version: "1.0.0",
		Icon:    "assets/icon.svg",
		Entry:   "https://files.apps.test/",
		Window: AppWindow{
			Width: 1024, Height: 720,
			MinWidth: 640, MinHeight: 480,
			Resizable: true, Mode: AppWindowSingle, DefaultState: AppStateNormal,
		},
		Permissions: []string{"clipboard-read", "file-selection"},
	}
}

// appDeclaration declares the given applications.
func appDeclaration(identifier string, apps ...App) Declaration {
	return Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		Contributions: Contributions{
			Apps: apps,
		},
	}
}

// TestLoadKeepsAWindowApplication checks that a declaration satisfying
// KOSD-C-73 loads and keeps every declared field.
func TestLoadKeepsAWindowApplication(t *testing.T) {
	declaration := appDeclaration("kos.files", validApp())
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("valid declaration has problems: %v", loaded.Problems)
	}
	if len(loaded.Contributions.Apps) != 1 {
		t.Fatalf("apps = %+v, want one", loaded.Contributions.Apps)
	}
	app := loaded.Contributions.Apps[0]
	if app.ID != "files" || app.Entry != "https://files.apps.test/" || app.Window.Width != 1024 {
		t.Fatalf("app = %+v, want every declared field", app)
	}
}

// TestLoadRejectsAnInvalidWindowApplication checks each KOSD-C-73 rule: an
// invalid application is absent and the class reports the problem (C-28).
func TestLoadRejectsAnInvalidWindowApplication(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*App)
	}{
		{"no id", func(app *App) { app.ID = "" }},
		{"untrimmed id", func(app *App) { app.ID = " files" }},
		{"no name", func(app *App) { app.Name = "" }},
		{"no version", func(app *App) { app.Version = "" }},
		{"no icon", func(app *App) { app.Icon = "" }},
		{"escaping icon", func(app *App) { app.Icon = "assets/../icon.svg" }},
		{"http entry", func(app *App) { app.Entry = "http://files.apps.test/" }},
		{"relative entry", func(app *App) { app.Entry = "files" }},
		{"entry credentials", func(app *App) { app.Entry = "https://user:pass@files.apps.test/" }},
		{"zero width", func(app *App) { app.Window.Width = 0 }},
		{"negative height", func(app *App) { app.Window.Height = -1 }},
		{"unknown mode", func(app *App) { app.Window.Mode = "tabbed" }},
		{"unknown default", func(app *App) { app.Window.DefaultState = "hidden" }},
		{"unknown permission", func(app *App) { app.Permissions = []string{"camera"} }},
		{"backend without type", func(app *App) { app.Backend = &AppBackend{Health: "/healthz"} }},
		{"empty deep link", func(app *App) { app.DeepLinks = []string{""} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			app := validApp()
			testCase.mutate(&app)
			loaded, err := appDeclaration("kos.files", app).Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(loaded.Contributions.Apps) != 0 || len(loaded.Problems) == 0 {
				t.Fatalf("loaded apps = %+v, problems = %v; want the invalid application dropped",
					loaded.Contributions.Apps, loaded.Problems)
			}
		})
	}
}

// TestLoadRejectsDuplicateApplicationIDs checks the uniqueness KOSD-C-73
// states inside one installed extension.
func TestLoadRejectsDuplicateApplicationIDs(t *testing.T) {
	loaded, err := appDeclaration("kos.files", validApp(), validApp()).Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Contributions.Apps) != 1 || len(loaded.Problems) == 0 {
		t.Fatalf("apps = %+v, problems = %v; want the duplicate dropped and the first kept",
			loaded.Contributions.Apps, loaded.Problems)
	}
}

// TestParseRequiresEveryAppField checks the required set is required in the
// document, not only by value (KOSD-C-73).
func TestParseRequiresEveryAppField(t *testing.T) {
	encoded, err := json.Marshal(appDeclaration("kos.files", validApp()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, field := range []string{"id", "name", "version", "icon", "entry", "window", "permissions"} {
		t.Run(field, func(t *testing.T) {
			document := decodeDocument(t, encoded)
			app := document["contributions"].(map[string]any)["apps"].([]any)[0].(map[string]any)
			delete(app, field)
			if _, err := Parse(encodeDocument(t, document)); err == nil {
				t.Fatalf("a declaration without %s parsed", field)
			}
		})
	}
	for _, field := range []string{"width", "height", "minWidth", "minHeight", "resizable", "mode", "defaultState"} {
		t.Run("window."+field, func(t *testing.T) {
			document := decodeDocument(t, encoded)
			window := document["contributions"].(map[string]any)["apps"].([]any)[0].(map[string]any)["window"].(map[string]any)
			delete(window, field)
			if _, err := Parse(encodeDocument(t, document)); err == nil {
				t.Fatalf("a declaration without window.%s parsed", field)
			}
		})
	}
	if _, err := Parse(encoded); err != nil {
		t.Fatalf("the complete declaration was refused: %v", err)
	}
}

// TestEmptyPermissionsAreValid checks that the field may be empty but not
// missing (KOSD-C-73).
func TestEmptyPermissionsAreValid(t *testing.T) {
	document := decodeDocument(t, emptyAppDocument(t))
	document["contributions"].(map[string]any)["apps"].([]any)[0].(map[string]any)["permissions"] = []any{}
	declaration, err := Parse(encodeDocument(t, document))
	if err != nil {
		t.Fatalf("an empty permission set was refused: %v", err)
	}
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Problems) != 0 || len(loaded.Contributions.Apps) != 1 {
		t.Fatalf("problems = %v, apps = %+v; want an empty permission set kept",
			loaded.Problems, loaded.Contributions.Apps)
	}
}

// TestPackageDigestCoversManifestAndAssets checks KOSD-C-17: changing the
// declaration or any single asset changes the package digest.
func TestPackageDigestCoversManifestAndAssets(t *testing.T) {
	declaration := appDeclaration("kos.files", validApp())
	icon := Asset{Path: "assets/icon.svg", Content: []byte("<svg/>")}
	archive, digest, err := Pack(declaration, []Asset{icon})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	artifact, err := Read(archive)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if artifact.Digest != digest {
		t.Fatalf("read digest = %q, pack digest = %q", artifact.Digest, digest)
	}

	_, changedAssetDigest, err := Pack(declaration, []Asset{{Path: "assets/icon.svg", Content: []byte("<svg width=\"1\"/>")}})
	if err != nil {
		t.Fatalf("pack changed asset: %v", err)
	}
	if changedAssetDigest == digest {
		t.Fatal("changing an asset did not change the package digest")
	}

	changedDeclaration := declaration
	changedDeclaration.Version = "1.0.1"
	_, changedDeclarationDigest, err := Pack(changedDeclaration, []Asset{icon})
	if err != nil {
		t.Fatalf("pack changed declaration: %v", err)
	}
	if changedDeclarationDigest == digest {
		t.Fatal("changing the declaration did not change the package digest")
	}

	wrong := Package{Reference: "x", Digest: "sha256:" + strings.Repeat("0", 64), Content: archive}
	if _, err := wrong.Verify(); err == nil {
		t.Fatal("a package matched a digest it does not carry")
	}
	if _, err := Read([]byte("this is not a package")); err == nil {
		t.Fatal("a non-archive was read as a package")
	}
}

// TestPackageStructureIsClosed checks that an entry outside the package
// structure refuses the package (KOSD-C-17).
func TestPackageStructureIsClosed(t *testing.T) {
	document, err := json.Marshal(appDeclaration("kos.files", validApp()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	digest := DigestOf(document, nil)
	closed := packEntries(t, [][2][]byte{
		{[]byte(ManifestEntry), document},
		{[]byte(DigestEntry), []byte(digest + "\n")},
	})
	if _, err := Read(closed); err != nil {
		t.Fatalf("read the closed package: %v", err)
	}
	open := packEntries(t, [][2][]byte{
		{[]byte(ManifestEntry), document},
		{[]byte("notes.txt"), []byte("extra")},
		{[]byte(DigestEntry), []byte(digest + "\n")},
	})
	if _, err := Read(open); err == nil {
		t.Fatal("a package carrying an undeclared entry was accepted")
	}
}

// TestCarriedAppsDropsAUndeclaredIcon checks KOSD-C-73's icon source rule.
func TestCarriedAppsDropsAUndeclaredIcon(t *testing.T) {
	app := validApp()
	app.Icon = "assets/icon.svg"
	kept, problems := carriedApps([]App{app}, []Asset{{Path: "assets/icon.svg", Content: []byte("<svg/>")}})
	if len(kept) != 1 || len(problems) != 0 {
		t.Fatalf("kept = %+v, problems = %v; want the carried icon kept", kept, problems)
	}
	kept, problems = carriedApps([]App{app}, nil)
	if len(kept) != 0 || len(problems) != 1 {
		t.Fatalf("kept = %+v, problems = %v; want the missing icon reported", kept, problems)
	}

	app.Icon = "https://files.apps.test/icon.svg"
	kept, problems = carriedApps([]App{app}, nil)
	if len(kept) != 1 || len(problems) != 0 {
		t.Fatalf("kept = %+v, problems = %v; want an origin icon kept", kept, problems)
	}
}

// decodeDocument decodes one JSON object.
func decodeDocument(t *testing.T, encoded []byte) map[string]any {
	t.Helper()
	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	return document
}

// encodeDocument encodes one JSON object.
func encodeDocument(t *testing.T, document map[string]any) []byte {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode document: %v", err)
	}
	return encoded
}

// emptyAppDocument builds a document without the permissions field, so a case
// can put an empty set back in its place.
func emptyAppDocument(t *testing.T) []byte {
	t.Helper()
	encoded, err := json.Marshal(appDeclaration("kos.files", validApp()))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	document := decodeDocument(t, encoded)
	app := document["contributions"].(map[string]any)["apps"].([]any)[0].(map[string]any)
	delete(app, "permissions")
	return encodeDocument(t, document)
}

// packEntries writes one archive from name and content pairs.
func packEntries(t *testing.T, entries [][2][]byte) []byte {
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
		t.Fatalf("close archive compression: %v", err)
	}
	return buffer.Bytes()
}
