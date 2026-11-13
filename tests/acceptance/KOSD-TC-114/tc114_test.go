// Package kosdtc114 checks KOSD-AC-56 on the file manager product and a
// third-party package: each takes effect only through its declared
// contributions, the kernel kind set, capability baseline, page shell and
// writer table stay as they were, the third-party package loads the way the
// first-party one does, and removing one product leaves the other serving
// (KOSD-TC-114).
package kosdtc114

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	filemanagerComponent  = "kos-filemanager"
	filemanagerPreference = "kos-filemanager"
	filemanagerApp        = "kos-filemanager"
	filemanagerEntry      = "https://filemanager.kos.local/"

	filemanagerExposedVolumes = "exposedVolumes"
	filemanagerView           = "view"
	filemanagerInitialPath    = "initialPath"

	thirdPartyComponent  = "third-party-notifier"
	thirdPartyGroup      = "notify.example.test"
	thirdPartyVersion    = "v1"
	thirdPartyKind       = "Notice"
	thirdPartyResource   = "notices"
	thirdPartyController = "notifier"

	thirdPartyChannels      = "thirdparty.notifier.channels"
	thirdPartySubscriptions = "thirdparty.notifier.subscriptions"
)

// filemanagerPackage is the first-party product this case installs, carrying
// the contributions its package ships: one window application and the
// preference domain its settings surface edits (C-17, C-18, KOSD-C-73). It is a
// fixture for this case, not a second implementation: the product lives in its
// own repository and the kernel loads only what the declaration names.
func filemanagerPackage() extension.Declaration {
	return extension.Declaration{
		Identifier:       filemanagerComponent,
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Preferences: []extension.PreferenceSchema{{
				ID:   filemanagerPreference,
				Keys: []string{filemanagerExposedVolumes, filemanagerView, filemanagerInitialPath},
			}},
			Apps: []extension.App{{
				ID:      filemanagerApp,
				Name:    "文件管理",
				Version: "1.0.0",
				Icon:    "assets/icon.svg",
				Entry:   filemanagerEntry,
				Window: extension.AppWindow{
					Width: 1200, Height: 800, MinWidth: 720, MinHeight: 480,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{"clipboard-read", "clipboard-write"},
				Backend:     &extension.AppBackend{Type: "http", Health: filemanagerEntry + "healthz"},
			}},
		},
		Permissions: []extension.Permission{{
			Contribution: filemanagerApp,
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

// thirdPartyDeclaration is an extension outside the distribution that
// contributes the same kinds of entries as a first-party product: one object
// class, one controller and two settings (C-18).
func thirdPartyDeclaration() extension.Declaration {
	return extension.Declaration{
		Identifier:       "thirdparty.notifier",
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		APIGroup:         thirdPartyGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: thirdPartyGroup, Kind: thirdPartyKind, Resource: thirdPartyResource,
				Versions: []string{thirdPartyVersion},
			}},
			Controllers: []extension.Controller{{
				ID:      thirdPartyController,
				Manages: []extension.GroupKind{{Group: thirdPartyGroup, Kind: thirdPartyKind}},
				Scope:   extension.WriteScope{Groups: []string{thirdPartyGroup}},
			}},
			Settings: []extension.Setting{
				{
					ID: thirdPartyChannels, Scope: extension.SettingScopeCluster,
					Writer: extension.SettingWriterUser, Editable: true,
				},
				{
					ID: thirdPartySubscriptions, Scope: extension.SettingScopeCluster,
					Writer: extension.SettingWriterUser, Editable: true,
				},
			},
		},
	}
}

// TestTC114ContributionsLoadAndWithdraw walks the install order and the removal
// order of both products, checking after each step what is served and that the
// kernel did not change.
func TestTC114ContributionsLoadAndWithdraw(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, thirdPartyGroup, thirdPartyVersion, thirdPartyKind, thirdPartyResource, false)
	artifacts := extension.MemoryArtifacts{}

	kindsBefore := append([]string(nil), api.Kinds()...)
	capabilitiesBefore := extension.BaselineCapabilities()
	ownedBefore := write.OwnedKinds()
	denialBefore := deniedKernelWrite(t, cluster)

	// The file manager product: its window application and its two display
	// preferences, and no page (the interface opens as an application window).
	installed := cluster.InstallPackageAssetsWith(t, filemanagerComponent, filemanagerPackage(), icon(), artifacts, nil)
	requireReady(t, installed, filemanagerComponent)
	registry := installed.Registry()
	keys, found := registry.PreferenceKeys(filemanagerPreference)
	if !found || !reflect.DeepEqual(keys, []string{filemanagerExposedVolumes, filemanagerView, filemanagerInitialPath}) {
		t.Fatalf("preference keys = %v (found %v), want the exposed volumes, view and initial path", keys, found)
	}
	requireApp(t, registry, filemanagerApp, 1)
	requireNoPages(t, registry)
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// A third-party package with the same kinds of contributions loads the same
	// way (C-17: the kernel loads declarations, not names).
	installed = cluster.InstallPackage(t, thirdPartyComponent, thirdPartyDeclaration(), artifacts)
	registry = installed.Registry()
	firstParty, found := installed.Component(filemanagerComponent)
	if !found {
		t.Fatal("the file manager component left the install result")
	}
	thirdParty, found := installed.Component(thirdPartyComponent)
	if !found {
		t.Fatal("the third-party component is missing from the install result")
	}
	if firstParty.Phase != thirdParty.Phase || len(firstParty.Problems) != len(thirdParty.Problems) {
		t.Fatalf("first-party %+v and third-party %+v loaded differently", firstParty, thirdParty)
	}
	if _, served := registry.ObjectClass(thirdPartyGroup, thirdPartyKind); !served {
		t.Fatal("the third-party class is not served after the install")
	}
	requireController(t, registry, thirdPartyController, thirdPartyGroup, thirdPartyKind)
	for _, id := range []string{thirdPartyChannels, thirdPartySubscriptions} {
		if _, found := registry.Setting(id); !found {
			t.Fatalf("the third-party setting %q is not visible", id)
		}
	}
	requireApp(t, registry, filemanagerApp, 1)
	if _, found := registry.PreferenceKeys(filemanagerPreference); !found {
		t.Fatal("the file manager preference domain disappeared when the second product installed")
	}
	requireNoPages(t, registry)
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// Removing the file manager withdraws its application and preferences and
	// leaves the third-party package serving.
	cluster.DeleteComponent(t, filemanagerComponent)
	registry = cluster.ExtensionState(t, artifacts).Registry()
	if _, found := registry.PreferenceKeys(filemanagerPreference); found {
		t.Fatal("the file manager preference schema survived the uninstall")
	}
	requireApp(t, registry, filemanagerApp, 0)
	if _, served := registry.ObjectClass(thirdPartyGroup, thirdPartyKind); !served {
		t.Fatal("the third-party class disappeared with another product's uninstall")
	}
	requireController(t, registry, thirdPartyController, thirdPartyGroup, thirdPartyKind)
	for _, id := range []string{thirdPartyChannels, thirdPartySubscriptions} {
		if _, found := registry.Setting(id); !found {
			t.Fatalf("the third-party setting %q disappeared with another product's uninstall", id)
		}
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// Removing the third-party package withdraws its class, controller and
	// settings, and the control plane stops serving its class (C-19).
	cluster.DeleteComponent(t, thirdPartyComponent)
	registry = cluster.ExtensionState(t, artifacts).Registry()
	if _, served := registry.ObjectClass(thirdPartyGroup, thirdPartyKind); served {
		t.Fatal("the third-party class is still served after the uninstall")
	}
	for _, controller := range registry.Controllers() {
		if controller.ID == thirdPartyController {
			t.Fatal("the third-party controller survived the uninstall")
		}
	}
	for _, id := range []string{thirdPartyChannels, thirdPartySubscriptions} {
		if _, found := registry.Setting(id); found {
			t.Fatalf("the third-party setting %q survived the uninstall", id)
		}
	}
	control := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Extensions = registry
	})
	status, body := control.Call(t, http.MethodGet,
		"/api/v1/resources/"+thirdPartyKind+"?apiVersion="+thirdPartyGroup+"/"+thirdPartyVersion, nil, nil)
	if status != http.StatusUnprocessableEntity || harness.String(body, "error", "code") != controlplane.CodeCapabilityMissing {
		t.Fatalf("read the withdrawn class = %d %v, want capability_missing", status, body)
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)
}

// requireReady checks one component installed with its contributions loaded.
func requireReady(t *testing.T, installation *extension.Installation, name string) {
	t.Helper()
	component, found := installation.Component(name)
	if !found {
		t.Fatalf("component %s is missing from the install result", name)
	}
	if component.Phase != extension.PhaseReady {
		t.Fatalf("component %s = %s (%s: %s), want ready", name, component.Phase, component.Reason, component.Message)
	}
}

// requireApp checks the projection carries one application id exactly count
// times, so a withdrawn product contributes none.
func requireApp(t *testing.T, registry *extension.Registry, id string, count int) {
	t.Helper()
	seen := 0
	for _, app := range registry.Apps() {
		if app.Manifest.ID == id {
			seen++
		}
	}
	if seen != count {
		t.Fatalf("the projection carries %d applications with id %s, want %d", seen, id, count)
	}
}

// requireController checks one controller contribution is loaded for a class.
func requireController(t *testing.T, registry *extension.Registry, id, group, kind string) {
	t.Helper()
	for _, controller := range registry.Controllers() {
		if controller.ID != id {
			continue
		}
		for _, class := range controller.Manages {
			if class.Group == group && class.Kind == kind {
				return
			}
		}
	}
	t.Fatalf("controller %s managing %s/%s is not loaded", id, group, kind)
}

// requireNoPages checks the products added no page to the shell.
func requireNoPages(t *testing.T, registry *extension.Registry) {
	t.Helper()
	if len(registry.Pages()) != 0 {
		t.Fatalf("the shell gained pages %+v from products that declare none", registry.Pages())
	}
}

// requireKernelUnchanged compares the kernel sets with the ones recorded before
// the first install, including the refusal an extension controller gets on a
// kernel write (C-01, C-09, C-19).
func requireKernelUnchanged(t *testing.T, kinds []string, capabilities []extension.Capability, owned []string, denial string, cluster *harness.Cluster) {
	t.Helper()
	if !reflect.DeepEqual(api.Kinds(), kinds) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
	if !reflect.DeepEqual(extension.BaselineCapabilities(), capabilities) {
		t.Fatal("the capability baseline changed")
	}
	if !reflect.DeepEqual(write.OwnedKinds(), owned) {
		t.Fatal("the writer table changed")
	}
	if observed := deniedKernelWrite(t, cluster); observed != denial {
		t.Fatalf("the kernel write refusal changed from %q to %q", denial, observed)
	}
}

// deniedKernelWrite attempts one extension controller write to a kernel class
// and returns the refusal, so the case can compare the writer table before and
// after installing products.
func deniedKernelWrite(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	writer := cluster.Writer()
	writer.Scope = extension.WriteScope{Groups: []string{thirdPartyGroup}}.Admit()
	_, err := writer.Create(context.Background(), write.SubjectExtensionReconciler, write.Session{}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "probe", "namespace": harness.Namespace},
		"spec":       map[string]any{"namespace": harness.Namespace},
	}})
	if err == nil {
		t.Fatal("an extension controller wrote a kernel object")
	}
	return err.Error()
}
