// Package kosdtc157 checks KOSD-AC-72's preinstalled-application path against
// the channel a distribution uses: an extension package is a file below the
// control plane's package directory, and the system component names that file
// relative to it (KOSD-C-17). Bytes other than the delivered ones, or a missing
// file, must make the case fail.
//
// The distribution step that creates the preinstalled component has no
// implementation on this revision; the case marks that dependency below.
package kosdtc157

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/install"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	preinstalledComponent = "kos-preinstalled"
	preinstalledID        = "kos-preinstalled"
	preinstalledApp       = "preinstalled"
	preinstalledEntry     = "https://preinstalled.kos.local/"

	thirdPartyComponent = "acme-notes"
	thirdPartyID        = "acme.notes"
	thirdPartyApp       = "notes"
	thirdPartyEntry     = "https://notes.example/"

	memberName = "alice"
	memberPass = "alice-password"
)

// appPackage is one application package. The distribution preinstalls the first
// one and a user installs the second; both carry the same declaration shape so
// the case compares like with like (KOSD-C-17, KOSD-C-18, KOSD-C-73).
func appPackage(identifier, appID, name, entry string) extension.Declaration {
	return extension.Declaration{
		Identifier:       identifier,
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Apps: []extension.App{{
				ID:      appID,
				Name:    name,
				Version: "1.0.0",
				Icon:    "https://icons.example/" + appID + ".svg",
				Entry:   entry,
				Window: extension.AppWindow{
					Width: 1000, Height: 700, MinWidth: 640, MinHeight: 480,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{},
			}},
		},
	}
}

// TestTC157DeliveredPackageIsTheInstalledApplication delivers the preinstalled
// package the way a distribution does — a file under the package directory the
// control plane mounts — and then checks the component's source is those bytes.
// A different artifact, or a missing one, must make the case fail.
func TestTC157DeliveredPackageIsTheInstalledApplication(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)

	// The delivered channel is the package directory the deployable control
	// plane mounts, not a store the case keeps in memory (KOSD-NG-7).
	requirePackageDirectoryMounted(t)
	packages := t.TempDir()
	reference, digest := deliver(t, packages, preinstalledComponent,
		appPackage(preinstalledID, preinstalledApp, "Preinstalled", preinstalledEntry))

	// TODO(TSI-4528): the distribution install step that creates this component
	// does not exist yet; once it lands, this creation moves into the installer.
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(preinstalledComponent, reference, digest)})
	installation := cluster.ExtensionStateWith(t, extension.DirectoryArtifacts{Root: packages}, shell.Retention())
	installed, found := installation.Component(preinstalledComponent)
	if !found {
		t.Fatalf("the install result carries no component %s", preinstalledComponent)
	}
	if installed.Phase != extension.PhaseReady {
		t.Fatalf("component %s = %s (%s: %s), want ready", preinstalledComponent, installed.Phase, installed.Reason, installed.Message)
	}
	if installed.Digest != digest {
		t.Fatalf("installed digest = %q, want the delivered artifact %q", installed.Digest, digest)
	}

	// The component's package reference resolves below the directory, and the
	// digest it names is the digest of the bytes on disk (KOSD-C-17).
	component := cluster.Get(t, "v1beta1", "SystemComponent", preinstalledComponent)
	if got := harness.String(component.Object, "spec", "package", "reference"); got != reference {
		t.Fatalf("package reference = %q, want the delivered %q", got, reference)
	}
	onDisk, err := os.ReadFile(filepath.Join(packages, filepath.FromSlash(reference)))
	if err != nil {
		t.Fatalf("read the delivered package: %v", err)
	}
	artifact, err := extension.Read(onDisk)
	if err != nil {
		t.Fatalf("read the delivered artifact: %v", err)
	}
	if got := harness.String(component.Object, "spec", "package", "digest"); got != artifact.Digest {
		t.Fatalf("package digest = %q, want the digest of the delivered bytes %q", got, artifact.Digest)
	}

	// A distribution that delivers other bytes under the same reference breaks
	// the component instead of installing the wrong artifact.
	other := appPackage(preinstalledID, preinstalledApp, "Preinstalled", "https://elsewhere.kos.local/")
	otherContent, _, err := extension.Pack(other, nil)
	if err != nil {
		t.Fatalf("pack the other artifact: %v", err)
	}
	writeFile(t, filepath.Join(packages, filepath.FromSlash(reference)), otherContent)
	installation = cluster.ExtensionStateWith(t, extension.DirectoryArtifacts{Root: packages}, shell.Retention())
	requireUnavailable(t, installation, preinstalledComponent, extension.ReasonDigestMismatch)

	// A distribution that delivers no file at all breaks it too, rather than
	// leaving a component that silently contributes nothing.
	if err := os.Remove(filepath.Join(packages, filepath.FromSlash(reference))); err != nil {
		t.Fatalf("remove the delivered package: %v", err)
	}
	installation = cluster.ExtensionStateWith(t, extension.DirectoryArtifacts{Root: packages}, shell.Retention())
	requireUnavailable(t, installation, preinstalledComponent, extension.ReasonPackageUnavailable)

	// Delivering the right bytes again restores the application.
	writeFile(t, filepath.Join(packages, filepath.FromSlash(reference)), onDisk)
	installation = cluster.ExtensionStateWith(t, extension.DirectoryArtifacts{Root: packages}, shell.Retention())
	installer, found := installation.Component(preinstalledComponent)
	if !found || installer.Phase != extension.PhaseReady || installer.Digest != digest {
		t.Fatalf("component after redelivery = %+v, want ready at %q", installer, digest)
	}
}

// TestTC157PreinstalledAppUsesTheSameChannel preinstalls one system application
// through the package directory, installs a third-party extension through the
// same directory afterwards, then uninstalls and reinstalls both individually.
func TestTC157PreinstalledAppUsesTheSameChannel(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	member := shell.Account(t, owner, memberName, memberPass, write.RoleMember)

	packages := t.TempDir()
	artifacts := extension.DirectoryArtifacts{Root: packages}
	preinstalledRef, preinstalledDigest := deliver(t, packages, preinstalledComponent,
		appPackage(preinstalledID, preinstalledApp, "Preinstalled", preinstalledEntry))
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(preinstalledComponent, preinstalledRef, preinstalledDigest)})
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))

	// The preinstalled application is a system component whose spec carries
	// nothing a later installation would not carry.
	component := cluster.Get(t, "v1beta1", "SystemComponent", preinstalledComponent)
	if got := specFields(t, component.Object); !reflect.DeepEqual(got, []string{"package"}) {
		t.Fatalf("system component spec fields = %v, want only the package; preinstallation added a field", got)
	}
	requireOnlyPackageSchema(t, cluster)

	// The application projection is the same one a third-party application
	// reaches: the desktop cannot tell a preinstalled application from a later
	// installation.
	status, body := shell.Call(t, http.MethodGet, "/api/v1/apps", nil, member)
	if status != http.StatusOK {
		t.Fatalf("read the preinstalled application = %d %v", status, body)
	}
	preinstalledItem := onlyApp(t, body, preinstalledComponent, preinstalledID, preinstalledApp)
	if got := harness.String(preinstalledItem, "digest"); got != preinstalledDigest {
		t.Fatalf("projected digest = %q, want %q", got, preinstalledDigest)
	}
	if got := harness.String(preinstalledItem, "phase"); got != string(extension.PhaseReady) {
		t.Fatalf("projected phase = %q, want ready", got)
	}
	if got := harness.String(preinstalledItem, "manifest", "entry"); got != preinstalledEntry {
		t.Fatalf("projected entry = %q, want %q", got, preinstalledEntry)
	}

	// There is no preinstall-only API: the application projection is read-only
	// and the guessed preinstall routes are the catch-all.
	status, payload, err := shell.Accounts.HTTP.Raw(http.MethodPost, "/api/v1/apps",
		map[string]any{"id": "x"}, harness.Bearer(owner))
	if err != nil {
		t.Fatalf("POST /api/v1/apps: %v", err)
	}
	if status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/v1/apps = %d %s, want the read-only projection", status, payload)
	}
	for _, path := range []string{
		"/api/v1/preinstall",
		"/api/v1/apps/preinstall",
		"/api/v1/resources/SystemComponent/" + preinstalledComponent + "/preinstall",
	} {
		if status, body = shell.Call(t, http.MethodGet, path, nil, owner); status != http.StatusNotFound {
			t.Fatalf("GET %s = %d %v, want the catch-all 404", path, status, body)
		}
	}

	// A third-party extension installs through the same package directory and
	// the same component path, so the two results are comparable.
	thirdPartyRef, thirdPartyDigest := deliver(t, packages, thirdPartyComponent,
		appPackage(thirdPartyID, thirdPartyApp, "Notes", thirdPartyEntry))
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(thirdPartyComponent, thirdPartyRef, thirdPartyDigest)})
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))

	status, body = shell.Call(t, http.MethodGet, "/api/v1/apps", nil, member)
	if status != http.StatusOK {
		t.Fatalf("read both applications = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 2 {
		t.Fatalf("the projection carries %d applications, want the preinstalled and the third-party one", len(items))
	}
	preinstalledItem = findApp(t, body, preinstalledComponent)
	thirdPartyItem := findApp(t, body, thirdPartyComponent)
	requireSameShape(t, preinstalledItem, thirdPartyItem)

	// The preinstalled application is uninstalled individually: only its
	// component, application and package go; the third-party one stays.
	cluster.DeleteComponent(t, preinstalledComponent)
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	status, body = shell.Call(t, http.MethodGet, "/api/v1/apps", nil, member)
	if status != http.StatusOK {
		t.Fatalf("read the projection after the individual uninstall = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 1 || harness.String(items[0], "component") != thirdPartyComponent {
		t.Fatalf("the projection after uninstalling the preinstalled application = %v, want only the third-party one", items)
	}
	if exists(t, cluster, preinstalledComponent) {
		t.Fatal("the preinstalled system component survived its individual uninstall")
	}
	if !exists(t, cluster, thirdPartyComponent) {
		t.Fatal("uninstalling the preinstalled application removed the third-party component")
	}

	// Reinstalling it goes through the same package directory and produces the
	// same projection as the third-party application.
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(preinstalledComponent, preinstalledRef, preinstalledDigest)})
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	status, body = shell.Call(t, http.MethodGet, "/api/v1/apps", nil, member)
	if status != http.StatusOK {
		t.Fatalf("read the projection after the reinstall = %d %v", status, body)
	}
	preinstalledItem = findApp(t, body, preinstalledComponent)
	thirdPartyItem = findApp(t, body, thirdPartyComponent)
	if got := harness.String(preinstalledItem, "digest"); got != preinstalledDigest {
		t.Fatalf("reinstalled digest = %q, want the same artifact %q", got, preinstalledDigest)
	}
	requireSameShape(t, preinstalledItem, thirdPartyItem)

	// Both leave the same way: removing the third-party extension removes only
	// its application, and removing the preinstalled one leaves an empty
	// projection.
	cluster.DeleteComponent(t, thirdPartyComponent)
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	status, body = shell.Call(t, http.MethodGet, "/api/v1/apps", nil, member)
	if status != http.StatusOK {
		t.Fatalf("read the projection after the third-party uninstall = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 1 || harness.String(items[0], "component") != preinstalledComponent {
		t.Fatalf("the projection after uninstalling the third-party application = %v, want only the preinstalled one", items)
	}
	cluster.DeleteComponent(t, preinstalledComponent)
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	status, body = shell.Call(t, http.MethodGet, "/api/v1/apps", nil, member)
	if status != http.StatusOK {
		t.Fatalf("read the empty projection = %d %v", status, body)
	}
	if items := harness.Items(t, body); len(items) != 0 {
		t.Fatalf("the projection after both uninstalls = %v, want none", items)
	}
}

// deliver writes one package into the package directory and returns the
// reference a component names and the digest the artifact carries.
func deliver(t *testing.T, root, name string, declaration extension.Declaration) (string, string) {
	t.Helper()
	content, digest, err := extension.Pack(declaration, nil)
	if err != nil {
		t.Fatalf("pack %s: %v", name, err)
	}
	reference := name + "-1.0.0.tar.gz"
	writeFile(t, filepath.Join(root, filepath.FromSlash(reference)), content)
	return reference, digest
}

// writeFile writes one delivered artifact and fails the case when it cannot.
func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// requirePackageDirectoryMounted checks the deployable control plane reads the
// package directory the case writes into, so the delivery channel under test is
// the shipped one (KOSD-C-17, KOSD-NG-7).
func requirePackageDirectoryMounted(t *testing.T) {
	t.Helper()
	documents, err := install.Render(harness.InstallerPlan(nil))
	if err != nil {
		t.Fatalf("render the installation: %v", err)
	}
	for _, document := range documents {
		if document.Object.GetKind() != "Deployment" || document.Object.GetName() != install.ControlPlaneName {
			continue
		}
		mounts, _, _ := unstructured.NestedSlice(document.Object.Object, "spec", "template", "spec", "containers")
		if len(mounts) != 1 {
			t.Fatalf("the control-plane workload carries %d containers, want one", len(mounts))
		}
		container, _ := mounts[0].(map[string]any)
		if !mountsAt(container["volumeMounts"], install.PackageDirectory) {
			t.Fatalf("the control plane mounts no volume at %s: %v", install.PackageDirectory, container["volumeMounts"])
		}
		args, _ := container["args"].([]any)
		if !containsValue(args, install.PackageDirectory) {
			t.Fatalf("the control plane does not read %s: %v", install.PackageDirectory, args)
		}
		return
	}
	t.Fatalf("the installation renders no %s Deployment", install.ControlPlaneName)
}

// mountsAt reports whether one container's mounts carry the mount path.
func mountsAt(raw any, path string) bool {
	mounts, _ := raw.([]any)
	for _, entry := range mounts {
		mount, _ := entry.(map[string]any)
		if mount["mountPath"] == path {
			return true
		}
	}
	return false
}

// containsValue reports whether one rendered list carries a value.
func containsValue(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// requireUnavailable checks one component failed for the expected reason.
func requireUnavailable(t *testing.T, installation *extension.Installation, name, reason string) {
	t.Helper()
	result, found := installation.Component(name)
	if !found {
		t.Fatalf("component %s is missing from the install result", name)
	}
	if result.Phase != extension.PhaseUnavailable || result.Reason != reason {
		t.Fatalf("component %s = %s (%s: %s), want unavailable with %s",
			name, result.Phase, result.Reason, result.Message, reason)
	}
}

// specFields returns the field names of one object's spec.
func specFields(t *testing.T, object map[string]any) []string {
	t.Helper()
	spec, ok := object["spec"].(map[string]any)
	if !ok {
		t.Fatalf("object has no spec: %v", object)
	}
	fields := make([]string, 0, len(spec))
	for field := range spec {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	return fields
}

// requireOnlyPackageSchema checks the served SystemComponent schema has no
// preinstall flag beside the package and the disable request.
func requireOnlyPackageSchema(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	crd, err := cluster.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().
		Get(context.Background(), "systemcomponents."+api.Group, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the SystemComponent definition: %v", err)
	}
	if len(crd.Spec.Versions) == 0 || crd.Spec.Versions[0].Schema == nil || crd.Spec.Versions[0].Schema.OpenAPIV3Schema == nil {
		t.Fatal("the SystemComponent definition carries no schema")
	}
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	fields := make([]string, 0, len(spec.Properties))
	for field := range spec.Properties {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	if want := []string{"disable", "package"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("SystemComponent spec fields = %v, want %v; a preinstall-only field appeared", fields, want)
	}
}

// onlyApp reads the one application in a projection and checks its attribution.
func onlyApp(t *testing.T, body map[string]any, component, identifier, app string) map[string]any {
	t.Helper()
	items := harness.Items(t, body)
	if len(items) != 1 {
		t.Fatalf("the projection carries %d applications, want one", len(items))
	}
	item := items[0]
	if got := harness.String(item, "component"); got != component {
		t.Fatalf("component = %q, want %q", got, component)
	}
	if got := harness.String(item, "extension"); got != identifier {
		t.Fatalf("extension = %q, want %q", got, identifier)
	}
	if got := harness.String(item, "manifest", "id"); got != app {
		t.Fatalf("application id = %q, want %q", got, app)
	}
	return item
}

// findApp returns the projection entry one component contributes.
func findApp(t *testing.T, body map[string]any, component string) map[string]any {
	t.Helper()
	for _, item := range harness.Items(t, body) {
		if harness.String(item, "component") == component {
			return item
		}
	}
	t.Fatalf("the projection carries no application from component %s: %v", component, body)
	return nil
}

// requireSameShape checks two projected applications carry the same fields, so
// the desktop reads a preinstalled and a third-party application the same way.
func requireSameShape(t *testing.T, left, right map[string]any) {
	t.Helper()
	if got, want := sortedKeys(left), sortedKeys(right); !reflect.DeepEqual(got, want) {
		t.Fatalf("application fields = %v, want the third-party fields %v", got, want)
	}
	leftManifest, _ := left["manifest"].(map[string]any)
	rightManifest, _ := right["manifest"].(map[string]any)
	if got, want := sortedKeys(leftManifest), sortedKeys(rightManifest); !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest fields = %v, want %v", got, want)
	}
	leftWindow, _ := leftManifest["window"].(map[string]any)
	rightWindow, _ := rightManifest["window"].(map[string]any)
	if got, want := sortedKeys(leftWindow), sortedKeys(rightWindow); !reflect.DeepEqual(got, want) {
		t.Fatalf("window fields = %v, want %v", got, want)
	}
}

// sortedKeys returns one object's field names in a stable order.
func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// exists reports whether one system component is still stored.
func exists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	_, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").
		Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read component %s: %v", name, err)
	}
	return true
}
