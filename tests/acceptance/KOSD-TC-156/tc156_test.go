// Package kosdtc156 checks KOSD-AC-72 on the file manager product: it installs
// as one extension package carrying its window application and its preference
// domain, the kernel keeps no file interface or file index of its own, its
// uninstall takes only the preference data the declaration gives up, and the
// volume ownership rules its data plane authorizes by stay the kernel's, with
// no product branch (KOSD-NG-10, KOSD-TC-156, FILE-C-1).
package kosdtc156

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	component   = "kos-filemanager"
	identifier  = "kos-filemanager"
	appID       = "kos-filemanager"
	appEntry    = "https://filemanager.kos.local/"
	appHealth   = appEntry + "healthz"
	preference  = "kos-filemanager"
	exposedKey  = "exposedVolumes"
	viewKey     = "view"
	pathKey     = "initialPath"
	unknownKey  = "session"
	otherDomain = "other.product"

	ownerName = "owner"
	ownerPass = "owner-password"
	aliceName = "alice"
	alicePass = "alice-password"
	bobName   = "bob"
	bobPass   = "bob-password"
	guestName = "guest"
	guestPass = "guest-password"
)

// filemanagerPackage is the package this case installs, carrying the
// contributions the product ships: one window application with its own backend
// and the preference domain its settings surface edits (C-17, C-18, KOSD-C-73).
// The data plane itself lives in the product's repository; the kernel loads
// only what the declaration names.
func filemanagerPackage() extension.Declaration {
	return extension.Declaration{
		Identifier:       identifier,
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Preferences: []extension.PreferenceSchema{{
				ID:   preference,
				Keys: []string{exposedKey, viewKey, pathKey},
			}},
			Apps: []extension.App{{
				ID:      appID,
				Name:    "文件管理",
				Version: "1.0.0",
				Icon:    "assets/icon.svg",
				Entry:   appEntry,
				Window: extension.AppWindow{
					Width: 1200, Height: 800, MinWidth: 720, MinHeight: 480,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{"clipboard-read", "clipboard-write"},
				Backend:     &extension.AppBackend{Type: "http", Health: appHealth},
			}},
		},
		Permissions: []extension.Permission{{
			Contribution: appID,
			ReadScope: extension.ReadScope{
				Groups: []string{api.Group},
				Kinds:  []extension.GroupKind{{Group: api.Group, Kind: "Volume"}},
			},
		}},
		Retention: []extension.Retention{{Resource: "preferences", Keep: false}},
	}
}

// icon is the single UI asset the package carries.
func icon() []extension.Asset {
	return []extension.Asset{{Path: "assets/icon.svg", Content: []byte("<svg><!-- file manager --></svg>")}}
}

// TestTC156FileManagerInstallsAsItsOwnPackage installs the product, reads what
// it contributes, stores the display preferences its settings surface edits, and
// removes it: the window and the preference domain follow the component, the
// kernel never serves a file path, and the declared retention takes the
// preference data.
func TestTC156FileManagerInstallsAsItsOwnPackage(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	ownerToken := shell.SignInOwner(t, ownerName, ownerPass)
	alice := shell.Account(t, ownerToken, aliceName, alicePass, write.RoleMember)
	bob := shell.Account(t, ownerToken, bobName, bobPass, write.RoleMember)
	guest := shell.Account(t, ownerToken, guestName, guestPass, write.RoleGuest)

	kindsBefore := append([]string(nil), api.Kinds()...)
	artifacts := extension.MemoryArtifacts{}
	installed := cluster.InstallPackageAssetsWith(t, component, filemanagerPackage(), icon(), artifacts, shell.Retention())
	shell.Use(installed)
	requireReady(t, installed)

	// The window application is projected with the manifest the package
	// declares, backend included: the data plane is the product's own service,
	// not a kernel surface (C-73).
	status, body := shell.Call(t, http.MethodGet, "/api/v1/apps", nil, alice)
	if status != http.StatusOK {
		t.Fatalf("read the application projection = %d %v", status, body)
	}
	items := harness.Items(t, body)
	if len(items) != 1 {
		t.Fatalf("the projection carries %d applications, want the file manager", len(items))
	}
	app := items[0]
	if harness.String(app, "extension") != identifier || harness.String(app, "component") != component {
		t.Fatalf("application = %v, want it attributed to the installed component", app)
	}
	for _, want := range []struct {
		path []string
		got  string
	}{
		{[]string{"manifest", "id"}, appID},
		{[]string{"manifest", "entry"}, appEntry},
		{[]string{"manifest", "backend", "type"}, "http"},
		{[]string{"manifest", "backend", "health"}, appHealth},
	} {
		if got := harness.String(app, want.path...); got != want.got {
			t.Fatalf("%v = %q, want %q", want.path, got, want.got)
		}
	}
	if width, _ := harness.Nested(app, "manifest", "window", "width").(float64); width != 1200 {
		t.Fatalf("window width = %v, want 1200", harness.Nested(app, "manifest", "window", "width"))
	}

	// The product declares a window, not a panel page, and no setting: its
	// contributions are exactly the ones the package names.
	registry := installed.Registry()
	if pages := registry.Pages(); len(pages) != 0 {
		t.Fatalf("the product added pages %+v; its interface is an application window", pages)
	}
	if settings := registry.Settings(); len(settings) != 0 {
		t.Fatalf("the product added settings %+v; its exposed volumes are display preferences", settings)
	}
	keys, found := registry.PreferenceKeys(preference)
	if !found || !reflect.DeepEqual(keys, []string{exposedKey, viewKey, pathKey}) {
		t.Fatalf("preference keys = %v (found %v), want the exposed volumes, view and initial path", keys, found)
	}

	// The display preferences are stored per account: naming another account's
	// volume is accepted as display state, and it is never served to that other
	// account (FILE-C-1: a preference does not widen authorization).
	status, body = shell.Call(t, http.MethodPut, "/api/v1/preferences/"+preference+"/"+exposedKey,
		map[string]any{"value": `["` + harness.Namespace + `/alice-docs","` + harness.Namespace + `/bob-data"]`}, alice)
	if status != http.StatusNoContent {
		t.Fatalf("store the exposed volumes = %d %v", status, body)
	}
	status, body = shell.Call(t, http.MethodPut, "/api/v1/preferences/"+preference+"/"+viewKey,
		map[string]any{"value": "grid"}, alice)
	if status != http.StatusNoContent {
		t.Fatalf("store the view = %d %v", status, body)
	}
	status, body = shell.Call(t, http.MethodGet, "/api/v1/preferences/"+preference, nil, alice)
	if status != http.StatusOK {
		t.Fatalf("read the preferences = %d %v", status, body)
	}
	if got := harness.String(body, "items", exposedKey); !strings.Contains(got, "bob-data") {
		t.Fatalf("stored exposed volumes = %q, want the display reference kept", got)
	}
	if got := harness.String(body, "items", viewKey); got != "grid" {
		t.Fatalf("stored view = %q, want grid", got)
	}
	status, body = shell.Call(t, http.MethodGet, "/api/v1/preferences/"+preference, nil, bob)
	if status != http.StatusOK {
		t.Fatalf("read another account's preferences = %d %v", status, body)
	}
	if got := harness.String(body, "items", exposedKey); got != "" {
		t.Fatalf("bob reads %q as his exposed volumes; another account's display state leaked", got)
	}

	// The domain is closed the way the declaration fixes it: an undeclared key
	// and an undeclared domain are refused with a reason, and a Guest writes
	// nothing (C-27, C-59).
	status, body = shell.Call(t, http.MethodPut, "/api/v1/preferences/"+preference+"/"+unknownKey,
		map[string]any{"value": "x"}, alice)
	if status != http.StatusBadRequest || harness.String(body, "error", "code") != controlplane.CodeInvalidField {
		t.Fatalf("write an undeclared key = %d %v, want invalid_field", status, body)
	}
	if message := harness.String(body, "error", "message"); !strings.Contains(message, unknownKey) {
		t.Fatalf("refusal message = %q, want it to name the undeclared key", message)
	}
	status, body = shell.Call(t, http.MethodPut, "/api/v1/preferences/"+otherDomain+"/"+viewKey,
		map[string]any{"value": "grid"}, alice)
	if status != http.StatusNotFound {
		t.Fatalf("write into an undeclared domain = %d %v, want 404", status, body)
	}
	status, body = shell.Call(t, http.MethodPut, "/api/v1/preferences/"+preference+"/"+viewKey,
		map[string]any{"value": "list"}, guest)
	if status != http.StatusForbidden || harness.String(body, "error", "code") != controlplane.CodePermissionDenied {
		t.Fatalf("a guest write = %d %v, want permission_denied", status, body)
	}

	// The kernel serves no file path and holds no file index: the data plane is
	// the product's own service (NG10).
	for _, path := range []string{
		"/files/window",
		"/files/volumes/" + harness.Namespace + "/alice-docs/entries?path=",
		"/files/volumes/" + harness.Namespace + "/alice-docs/download?path=notes.txt",
	} {
		if status, body = shell.Call(t, http.MethodGet, path, nil, alice); status != http.StatusNotFound {
			t.Fatalf("the kernel answered %s with %d %v, want 404", path, status, body)
		}
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed with the product installed: %v", api.Kinds())
	}

	// A volume the account owns lives in the kernel; the product's data plane
	// reads it, and removing the product leaves it alone.
	cluster.Create(t, harness.Volume(harness.Namespace, "alice-docs", map[string]any{
		"mobility":     "cluster-movable",
		"capacity":     "1Gi",
		"storageClass": "standard",
		"owner":        aliceName,
	}))

	// Removing the product withdraws its window and its preference domain, and
	// the declared retention takes the stored display state (C-17, C-29).
	cluster.DeleteComponent(t, component)
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	status, body = shell.Call(t, http.MethodGet, "/api/v1/apps", nil, alice)
	if status != http.StatusOK {
		t.Fatalf("read the application projection after the uninstall = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 0 {
		t.Fatalf("the applications survived the uninstall: %v", items)
	}
	status, body = shell.Call(t, http.MethodGet, "/api/v1/preferences/"+preference, nil, alice)
	if status != http.StatusNotFound {
		t.Fatalf("read the withdrawn preference domain = %d %v, want 404", status, body)
	}
	if kept := cluster.Get(t, "v1beta1", "Volume", "alice-docs"); harness.String(kept.Object, "spec", "owner") != aliceName {
		t.Fatal("the volume changed with the product's uninstall")
	}

	// Installing the product again brings the window back; the display state it
	// does not keep is gone rather than resurrected.
	installed = cluster.InstallPackageAssetsWith(t, component, filemanagerPackage(), icon(), artifacts, shell.Retention())
	shell.Use(installed)
	requireReady(t, installed)
	status, body = shell.Call(t, http.MethodGet, "/api/v1/preferences/"+preference, nil, alice)
	if status != http.StatusOK {
		t.Fatalf("read the preferences after the reinstall = %d %v", status, body)
	}
	if got := harness.String(body, "items", viewKey); got != "" {
		t.Fatalf("the reinstalled product served the old view %q; the declaration keeps no preference data", got)
	}
}

// TestTC156VolumeAuthorizationStaysTheKernelRule checks the ownership facts the
// file manager's data plane authorizes by: the API stamps the volume's owner,
// a Member writes its own volume and is refused another's with a reason, a
// Guest writes none, and installing or removing the product changes none of
// it (KOSD-C-59, KOSD-C-60, FILE-C-1).
func TestTC156VolumeAuthorizationStaysTheKernelRule(t *testing.T) {
	cluster := harness.Start(t)
	system := cluster.StartAccess(t, nil, nil)
	ownerToken := system.Owner(t, ownerName, ownerPass)
	alice := system.Account(t, ownerToken, aliceName, alicePass, write.RoleMember)
	system.Account(t, ownerToken, bobName, bobPass, write.RoleMember)
	guest := system.Account(t, ownerToken, guestName, guestPass, write.RoleGuest)

	cluster.Create(t, harness.Volume(harness.Namespace, "bob-data", map[string]any{
		"mobility":     "cluster-movable",
		"capacity":     "1Gi",
		"storageClass": "standard",
		"owner":        bobName,
	}))
	cluster.Create(t, harness.Volume(harness.Namespace, "guest-data", map[string]any{
		"mobility":     "cluster-movable",
		"capacity":     "1Gi",
		"storageClass": "standard",
		"owner":        guestName,
	}))
	namespaceParam := "?namespace=" + harness.Namespace
	volumes := "/api/v1/resources/Volume"
	// create names the attribution a new object carries; an update submits the
	// desired state only, so the markers the object already carries stay (C-02).
	create := func(name string, spec map[string]any) map[string]any {
		object := harness.Manifest("Volume", harness.Namespace, name, map[string]string{"managed-by": "kosd"}, nil)
		object["spec"] = spec
		return object
	}
	rewrite := func(name string) map[string]any {
		object := harness.Manifest("Volume", harness.Namespace, name, nil, nil)
		object["spec"] = map[string]any{"capacity": "2Gi"}
		return object
	}

	// A Member creates a volume: the API writes the session into spec.owner.
	status, answer := system.Call(t, alice, http.MethodPost, volumes+namespaceParam,
		create("alice-docs", map[string]any{"mobility": "cluster-movable", "capacity": "1Gi", "storageClass": "standard"}), nil)
	if status != http.StatusAccepted {
		t.Fatalf("a member creates a volume = %d %v", status, answer)
	}
	if got := harness.String(cluster.Get(t, "v1beta1", "Volume", "alice-docs").Object, "spec", "owner"); got != aliceName {
		t.Fatalf("spec.owner = %q, want the creating account %q", got, aliceName)
	}

	// The same Member writing another account's volume is refused, and the
	// refusal names the object it refused.
	refusal := func() (int, map[string]any) {
		return system.Call(t, alice, http.MethodPut, volumes+"/bob-data"+namespaceParam,
			rewrite("bob-data"),
			map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Volume", "bob-data").GetResourceVersion()})
	}
	status, answer = refusal()
	if status != http.StatusForbidden || harness.String(answer, "error", "code") != controlplane.CodePermissionDenied {
		t.Fatalf("a member writes another account's volume = %d %v, want permission_denied", status, answer)
	}
	if message := harness.String(answer, "error", "message"); !strings.Contains(message, "Volume/"+harness.Namespace+"/bob-data") {
		t.Fatalf("refusal message = %q, want it to name the volume it refused", message)
	}
	if got := harness.String(cluster.Get(t, "v1beta1", "Volume", "bob-data").Object, "spec", "owner"); got != bobName {
		t.Fatalf("spec.owner after the refused write = %q, want %q", got, bobName)
	}

	// A Guest writes nothing; the global Owner writes every volume.
	status, answer = system.Call(t, guest, http.MethodPut, volumes+"/guest-data"+namespaceParam,
		rewrite("guest-data"),
		map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Volume", "guest-data").GetResourceVersion()})
	if status != http.StatusForbidden || harness.String(answer, "error", "code") != controlplane.CodePermissionDenied {
		t.Fatalf("a guest writes its own volume = %d %v, want permission_denied", status, answer)
	}
	status, answer = system.Call(t, ownerToken, http.MethodPut, volumes+"/bob-data"+namespaceParam,
		rewrite("bob-data"),
		map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Volume", "bob-data").GetResourceVersion()})
	if status != http.StatusAccepted {
		t.Fatalf("the global Owner writes a member's volume = %d %v", status, answer)
	}

	// The authorization answers the same with the product installed and after
	// it is removed: no product branch decides who may touch a volume.
	artifacts := extension.MemoryArtifacts{}
	installed := cluster.InstallPackageAssetsWith(t, component, filemanagerPackage(), icon(), artifacts, nil)
	requireReady(t, installed)
	if status, answer = refusal(); status != http.StatusForbidden || harness.String(answer, "error", "code") != controlplane.CodePermissionDenied {
		t.Fatalf("a member writes another account's volume with the product installed = %d %v, want permission_denied", status, answer)
	}
	cluster.DeleteComponent(t, component)
	cluster.ExtensionState(t, artifacts)
	if status, answer = refusal(); status != http.StatusForbidden || harness.String(answer, "error", "code") != controlplane.CodePermissionDenied {
		t.Fatalf("a member writes another account's volume after the uninstall = %d %v, want permission_denied", status, answer)
	}
}

// requireReady checks the product installed with its contributions loaded.
func requireReady(t *testing.T, installation *extension.Installation) {
	t.Helper()
	installed, found := installation.Component(component)
	if !found {
		t.Fatalf("component %s is missing from the install result", component)
	}
	if installed.Phase != extension.PhaseReady {
		t.Fatalf("component %s = %s (%s: %s), want ready", installed.Name, installed.Phase, installed.Reason, installed.Message)
	}
}
