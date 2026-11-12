// Package kosdtc155 verifies that a first-party system application installs as
// an extension package, appears in the control-plane application projection with
// its declared manifest, and disappears with its component (KOSD-AC-72,
// KOSD-TC-155).
package kosdtc155

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	fileExtension = "kos.filemanager"
	fileComponent = "kos-filemanager"
	fileApp       = "files"
	fileEntry     = "https://files.apps.kos.example/"

	notesExtension = "acme.notes"
	notesComponent = "notes"
	notesApp       = "notes"
	notesEntry     = "https://notes.apps.example/"
)

// filemanager is the system application package this case installs: one window
// application and nothing else.
func filemanager() extension.Declaration {
	return extension.Declaration{
		Identifier:       fileExtension,
		Version:          "1.2.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Apps: []extension.App{{
				ID:      fileApp,
				Name:    "File Manager",
				Version: "1.2.0",
				Icon:    "assets/icon.svg",
				Entry:   fileEntry,
				Window: extension.AppWindow{
					Width: 1280, Height: 800, MinWidth: 720, MinHeight: 480,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{"clipboard-read", "file-selection", "external-link"},
				DeepLinks:   []string{"files://open"},
			}},
		},
	}
}

// notes is a second product contributing a page, a setting and one window
// application, so the case can prove an uninstall leaves the others intact.
func notes() extension.Declaration {
	return extension.Declaration{
		Identifier: notesExtension,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{{
				ID: "notes", Title: "Notes", Location: "panel",
				ReadOnly: true, ReadOnlyReason: "只读展示",
				Items: []extension.PageItem{{
					ID: "notes", Label: "Notes", Source: extension.PageSourceLocal, Store: "notes",
				}},
			}},
			Settings: []extension.Setting{{
				ID: "notes-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser,
			}},
			Apps: []extension.App{{
				ID:      notesApp,
				Name:    "Notes",
				Version: "1.0.0",
				Icon:    notesEntry + "icon.svg",
				Entry:   notesEntry,
				Window: extension.AppWindow{
					Width: 900, Height: 600, MinWidth: 480, MinHeight: 320,
					Resizable: true, Mode: extension.AppWindowMulti, DefaultState: extension.AppStateMaximized,
				},
				Permissions: []string{},
			}},
		},
	}
}

// icon is the file manager's single UI asset.
func icon() []extension.Asset {
	return []extension.Asset{{Path: "assets/icon.svg", Content: []byte("<svg><!-- file manager --></svg>")}}
}

// listApps reads the application projection as one session.
func listApps(t *testing.T, shell *harness.Shell, token string) []map[string]any {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/apps", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list applications = %d %v", status, body)
	}
	return harness.Items(t, body)
}

// appNamed returns the projection item declaring one application id.
func appNamed(items []map[string]any, id string) (map[string]any, bool) {
	for _, item := range items {
		manifest, _ := item["manifest"].(map[string]any)
		if harness.String(manifest, "id") == id {
			return item, true
		}
	}
	return nil, false
}

// TestTC155SystemApplicationAppearsAndDisappears installs two products, reads
// the application projection, then uninstalls one and reads the rest.
func TestTC155SystemApplicationAppearsAndDisappears(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	kindsBefore := api.Kinds()
	artifacts := extension.MemoryArtifacts{}

	installation := cluster.InstallPackageAssetsWith(t, fileComponent, filemanager(), icon(), artifacts, shell.Retention())
	shell.Use(installation)
	result, found := installation.Component(fileComponent)
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want ready", result, found)
	}

	items := listApps(t, shell, owner)
	if len(items) != 1 {
		t.Fatalf("applications = %v, want the installed one", items)
	}
	installed, found := appNamed(items, fileApp)
	if !found {
		t.Fatalf("applications = %v, want the declared application", items)
	}
	if got := harness.String(installed, "extension"); got != fileExtension {
		t.Fatalf("extension = %q, want %q", got, fileExtension)
	}
	if got := harness.String(installed, "component"); got != fileComponent {
		t.Fatalf("component = %q, want %q", got, fileComponent)
	}
	if got := harness.String(installed, "phase"); got != extension.PhaseReady {
		t.Fatalf("phase = %q, want %q", got, extension.PhaseReady)
	}
	if got := harness.String(installed, "digest"); got != harness.PackageDigest(t, artifacts["test://"+fileComponent]) {
		t.Fatalf("digest = %q, want the verified package digest", got)
	}
	assertFilemanagerManifest(t, installed)
	if apps := installation.Registry().AppsOf(fileExtension); len(apps) != 1 || apps[0].Manifest.ID != fileApp {
		t.Fatalf("AppsOf(%s) = %+v, want the declared application", fileExtension, apps)
	}

	installation = cluster.InstallPackageWith(t, notesComponent, notes(), artifacts, shell.Retention())
	shell.Use(installation)
	if pages := installation.Registry().Pages(); len(pages) != 1 || pages[0].ID != "notes" {
		t.Fatalf("pages = %+v, want the notes page", pages)
	}
	if _, found := installation.Registry().Setting("notes-endpoint"); !found {
		t.Fatal("the notes setting is not visible after the install")
	}
	items = listApps(t, shell, owner)
	if len(items) != 2 {
		t.Fatalf("applications = %v, want both products", items)
	}
	if _, found := appNamed(items, notesApp); !found {
		t.Fatalf("applications = %v, want the second product", items)
	}

	// Uninstall the file manager: its application disappears and the other
	// product keeps its page, setting and application.
	cluster.DeleteComponent(t, fileComponent)
	installation = cluster.ExtensionState(t, artifacts)
	shell.Use(installation)
	items = listApps(t, shell, owner)
	if len(items) != 1 {
		t.Fatalf("applications after the uninstall = %v, want the remaining product", items)
	}
	if _, found := appNamed(items, fileApp); found {
		t.Fatal("the uninstalled application is still projected")
	}
	if _, found := appNamed(items, notesApp); !found {
		t.Fatal("the other product's application disappeared with the uninstall")
	}
	if apps := installation.Registry().AppsOf(fileExtension); len(apps) != 0 {
		t.Fatalf("AppsOf(%s) after the uninstall = %+v, want none", fileExtension, apps)
	}
	if pages := installation.Registry().Pages(); len(pages) != 1 || pages[0].ID != "notes" {
		t.Fatalf("pages after the uninstall = %+v, want the notes page", pages)
	}
	if _, found := installation.Registry().Setting("notes-endpoint"); !found {
		t.Fatal("the other product's setting disappeared with the uninstall")
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
}

// assertFilemanagerManifest checks the projected manifest against the literals
// the declaration fixes (KOSD-C-73).
func assertFilemanagerManifest(t *testing.T, installed map[string]any) {
	t.Helper()
	manifest, ok := installed["manifest"].(map[string]any)
	if !ok {
		t.Fatalf("installed app = %v, want a manifest", installed)
	}
	for field, want := range map[string]string{
		"id": fileApp, "name": "File Manager", "version": "1.2.0",
		"icon": "assets/icon.svg", "entry": fileEntry,
	} {
		if got := harness.String(manifest, field); got != want {
			t.Fatalf("manifest.%s = %q, want %q", field, got, want)
		}
	}
	window, ok := manifest["window"].(map[string]any)
	if !ok {
		t.Fatalf("manifest = %v, want a window declaration", manifest)
	}
	for field, want := range map[string]float64{"width": 1280, "height": 800, "minWidth": 720, "minHeight": 480} {
		if got, _ := window[field].(float64); got != want {
			t.Fatalf("window.%s = %v, want %v", field, got, want)
		}
	}
	if resizable, _ := window["resizable"].(bool); !resizable {
		t.Fatal("window.resizable = false, want true")
	}
	if got := harness.String(window, "mode"); got != "single" {
		t.Fatalf("window.mode = %q, want single", got)
	}
	if got := harness.String(window, "defaultState"); got != "normal" {
		t.Fatalf("window.defaultState = %q, want normal", got)
	}
	permissions, _ := manifest["permissions"].([]any)
	wantPermissions := []string{"clipboard-read", "file-selection", "external-link"}
	if len(permissions) != len(wantPermissions) {
		t.Fatalf("permissions = %v, want %v", permissions, wantPermissions)
	}
	for index, want := range wantPermissions {
		if permissions[index] != want {
			t.Fatalf("permissions = %v, want %v", permissions, wantPermissions)
		}
	}
	deepLinks, _ := manifest["deepLinks"].([]any)
	if len(deepLinks) != 1 || deepLinks[0] != "files://open" {
		t.Fatalf("deepLinks = %v, want the declared link", deepLinks)
	}
	if _, present := manifest["backend"]; present {
		t.Fatalf("manifest = %v, want a UI-only application", manifest)
	}
}

// TestTC155ApplicationListFollowsTheSessionRole checks C-59: every signed-in
// role reads the installed set and an unsigned request is refused.
func TestTC155ApplicationListFollowsTheSessionRole(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackageAssetsWith(t, fileComponent, filemanager(), icon(), artifacts, shell.Retention())
	shell.Use(installation)

	status, body := shell.Call(t, http.MethodGet, "/api/v1/apps", nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("unsigned list = %d %v, want %d", status, body, http.StatusUnauthorized)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeUnauthenticated {
		t.Fatalf("unsigned list code = %q, want %q", code, controlplane.CodeUnauthenticated)
	}

	tokens := map[string]string{
		"owner":  owner,
		"member": shell.Account(t, owner, "member", "member-password", write.RoleMember),
		"guest":  shell.Account(t, owner, "guest", "guest-password", write.RoleGuest),
	}
	for role, token := range tokens {
		if items := listApps(t, shell, token); len(items) != 1 {
			t.Fatalf("%s sees applications %v, want the installed one", role, items)
		}
	}
}
