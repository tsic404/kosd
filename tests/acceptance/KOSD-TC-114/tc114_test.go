// Package kosdtc114 checks KOSD-AC-56 on the file manager product and a
// third-party package: each takes effect only through its declared
// contributions, the kernel kind set, capability baseline, page shell and
// writer table stay as they were, the third-party package loads the way the
// first-party one does, and removing one product leaves the other serving
// (KOSD-TC-114).
package kosdtc114

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/filemanager"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	filemanagerComponent = "kos-filemanager"
	thirdPartyComponent  = "third-party-notifier"
	thirdPartyGroup      = "notify.example.test"
	thirdPartyVersion    = "v1"
	thirdPartyKind       = "Notice"
	thirdPartyResource   = "notices"
	thirdPartyController = "notifier"

	thirdPartyChannels      = "thirdparty.notifier.channels"
	thirdPartySubscriptions = "thirdparty.notifier.subscriptions"
)

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

	// The file manager product: its user setting and its two preference keys,
	// and no page (the interface opens as an application window).
	installed := cluster.InstallPackage(t, filemanagerComponent, filemanager.Declaration(), artifacts)
	requireReady(t, installed, filemanagerComponent)
	registry := installed.Registry()
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); !found {
		t.Fatal("the exposed-volume setting is not visible after the file manager install")
	}
	keys, found := registry.PreferenceKeys(filemanager.Identifier)
	if !found || !reflect.DeepEqual(keys, []string{filemanager.PreferenceView, filemanager.PreferenceInitialPath}) {
		t.Fatalf("preference keys = %v (found %v), want the view and initial path", keys, found)
	}
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
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); !found {
		t.Fatal("the file manager setting disappeared when the second product installed")
	}
	requireNoPages(t, registry)
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// Removing the file manager withdraws its setting and preferences and
	// leaves the third-party package serving.
	cluster.DeleteComponent(t, filemanagerComponent)
	registry = cluster.ExtensionState(t, artifacts).Registry()
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); found {
		t.Fatal("the exposed-volume setting survived the uninstall")
	}
	if _, found := registry.PreferenceKeys(filemanager.Identifier); found {
		t.Fatal("the file manager preference schema survived the uninstall")
	}
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

func TestTC114FileManagerServesOnlyExposedOwnedVolumes(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	ownerToken := shell.SignInOwner(t, "owner", "owner-password")
	alice := createAccount(t, shell, ownerToken, "alice", "alice-password", write.RoleMember)
	bob := createAccount(t, shell, ownerToken, "bob", "bob-password", write.RoleMember)
	artifacts := extension.MemoryArtifacts{}
	installed := cluster.InstallPackage(t, filemanagerComponent, filemanager.Declaration(), artifacts)
	shell.Use(installed)
	requireReady(t, installed, filemanagerComponent)

	// Two volumes with different owners and the directories the data plane
	// serves for them.
	root := t.TempDir()
	for _, volume := range []struct{ name, owner string }{
		{"alice-docs", "alice"},
		{"bob-data", "bob"},
	} {
		cluster.Create(t, harness.Volume(harness.Namespace, volume.name, map[string]any{
			"mobility":     "cluster-movable",
			"capacity":     "1Gi",
			"storageClass": "standard",
			"owner":        volume.owner,
		}))
	}
	aliceRoot := filepath.Join(root, harness.Namespace, "alice-docs")
	if err := os.MkdirAll(filepath.Join(aliceRoot, "photos"), 0o755); err != nil {
		t.Fatalf("prepare the volume directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, harness.Namespace, "bob-data"), 0o755); err != nil {
		t.Fatalf("prepare the second volume directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aliceRoot, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("prepare the volume file: %v", err)
	}

	service := filemanager.New(filemanager.Config{
		Settings:    shell.Accounts.Store,
		Preferences: shell.Accounts.Service,
		Volumes:     filemanager.ClusterVolumes{Writer: cluster.Writer()},
		Roots:       filemanager.DirectoryRoots{Base: root},
	})
	dataPlane := httptest.NewServer(shell.Accounts.API.Handler(service.Handler()))
	t.Cleanup(dataPlane.Close)

	// Alice exposes her own volume and stores the window's preferences.
	writeSetting(t, shell, alice, filemanager.SettingExposedVolumes, `["`+harness.Namespace+`/alice-docs"]`)
	writePreference(t, shell, alice, filemanager.PreferenceView, filemanager.ViewGrid)
	writePreference(t, shell, alice, filemanager.PreferenceInitialPath, "photos")

	status, body := dataCall(t, dataPlane.URL, http.MethodGet, "/files/window", nil, "", alice)
	if status != http.StatusOK {
		t.Fatalf("open the window = %d %s", status, body)
	}
	window := decodeWindow(t, body)
	if window.View != filemanager.ViewGrid || window.InitialPath != "photos" {
		t.Fatalf("window = %+v, want the stored view and initial path", window)
	}
	if len(window.Volumes) != 1 || window.Volumes[0].Name != "alice-docs" {
		t.Fatalf("window volumes = %+v, want the one exposed volume", window.Volumes)
	}

	// Browsing, download, upload, rename and delete all stay inside the volume.
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/alice-docs", nil, "", alice)
	if status != http.StatusOK {
		t.Fatalf("list the volume = %d %s", status, body)
	}
	if !strings.Contains(string(body), `"notes.txt"`) {
		t.Fatalf("listing = %s, want the file", body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/alice-docs/content?path=notes.txt", nil, "", alice)
	if status != http.StatusOK || string(body) != "hello" {
		t.Fatalf("download = %d %q, want the stored content", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodPut, "/files/"+harness.Namespace+"/alice-docs/content?path=photos/new.txt", []byte("photo"), "application/octet-stream", alice)
	if status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, body)
	}
	rename := []byte(`{"from":"photos/new.txt","to":"photos/renamed.txt"}`)
	status, body = dataCall(t, dataPlane.URL, http.MethodPost, "/files/"+harness.Namespace+"/alice-docs/rename", rename, "application/json", alice)
	if status != http.StatusOK {
		t.Fatalf("rename = %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(aliceRoot, "photos", "renamed.txt")); err != nil {
		t.Fatalf("the renamed file is missing: %v", err)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodDelete, "/files/"+harness.Namespace+"/alice-docs/content?path=photos/renamed.txt", nil, "", alice)
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", status, body)
	}

	// A path outside the volume is refused instead of normalised (NG10).
	for _, path := range []string{"../../etc/passwd", "/etc/passwd"} {
		status, body = dataCall(t, dataPlane.URL, http.MethodGet,
			"/files/"+harness.Namespace+"/alice-docs/content?path="+path, nil, "", alice)
		if status != http.StatusBadRequest {
			t.Fatalf("download %q = %d %s, want 400", path, status, body)
		}
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodPut,
		"/files/"+harness.Namespace+"/alice-docs/content?path=../escape.txt", []byte("x"), "application/octet-stream", alice)
	if status != http.StatusBadRequest {
		t.Fatalf("upload outside the volume = %d %s, want 400", status, body)
	}

	// A symbolic link inside the volume is not a way out of it: the data plane
	// addresses the volume through a root handle, which refuses a path that
	// leaves it (NG10).
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside-secret"), 0o644); err != nil {
		t.Fatalf("prepare the file outside the volume: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(aliceRoot, "link")); err != nil {
		t.Fatalf("plant the link inside the volume: %v", err)
	}
	linked := "/files/" + harness.Namespace + "/alice-docs/content?path=link/secret.txt"
	if status, body = dataCall(t, dataPlane.URL, http.MethodGet, linked, nil, "", alice); status == http.StatusOK {
		t.Fatalf("a link inside the volume served a file outside it: %d %s", status, body)
	}
	if status, body = dataCall(t, dataPlane.URL, http.MethodPut, linked, []byte("pwned"), "application/octet-stream", alice); status == http.StatusCreated {
		t.Fatalf("an upload through the link was accepted: %d %s", status, body)
	}
	if status, body = dataCall(t, dataPlane.URL, http.MethodDelete, linked, nil, "", alice); status == http.StatusNoContent {
		t.Fatalf("a delete through the link was accepted: %d %s", status, body)
	}
	if content, err := os.ReadFile(outsideFile); err != nil || string(content) != "outside-secret" {
		t.Fatalf("the file outside the volume changed: %q, %v", content, err)
	}
	if status, body = dataCall(t, dataPlane.URL, http.MethodGet,
		"/files/"+harness.Namespace+"/alice-docs?path=link", nil, "", alice); status == http.StatusOK {
		t.Fatalf("the link was listed as a directory of the volume: %d %s", status, body)
	}

	// A file name with a quote or a non-ASCII character still produces a header
	// the client can parse.
	oddName := `报价"单".txt`
	if err := os.WriteFile(filepath.Join(aliceRoot, oddName), []byte("odd"), 0o644); err != nil {
		t.Fatalf("prepare the odd file: %v", err)
	}
	disposition, downloaded := downloadHeader(t, dataPlane.URL, alice,
		"/files/"+harness.Namespace+"/alice-docs/content?path="+url.QueryEscape(oddName))
	if string(downloaded) != "odd" {
		t.Fatalf("download %q = %q", oddName, downloaded)
	}
	mediaType, parameters, err := mime.ParseMediaType(disposition)
	if err != nil || mediaType != "attachment" || parameters["filename"] != oddName {
		t.Fatalf("Content-Disposition = %q (%v), want the file name", disposition, err)
	}

	// A volume the account did not expose is simply absent; exposing another
	// account's volume is refused by the owner rule (C-60).
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", alice)
	if status != http.StatusNotFound {
		t.Fatalf("list an unexposed volume = %d %s, want 404", status, body)
	}
	writeSetting(t, shell, alice, filemanager.SettingExposedVolumes,
		`["`+harness.Namespace+`/alice-docs","`+harness.Namespace+`/bob-data"]`)
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", alice)
	if status != http.StatusForbidden || !strings.Contains(string(body), controlplane.CodePermissionDenied) {
		t.Fatalf("list another account's volume = %d %s, want permission_denied", status, body)
	}
	writeSetting(t, shell, bob, filemanager.SettingExposedVolumes, `["`+harness.Namespace+`/bob-data"]`)
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", bob)
	if status != http.StatusOK {
		t.Fatalf("list the second account's own volume = %d %s, want 200", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", alice)
	if status != http.StatusForbidden {
		t.Fatalf("list the second account's volume as another member = %d %s, want 403", status, body)
	}

	// The data plane authenticates the session itself (C-46): a request without
	// one is refused, and a forwarding header does not stand in for it.
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/window", nil, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("open the window without a session = %d %s, want 401", status, body)
	}
	forged := map[string]string{"X-Forwarded-User": "alice", "X-Auth-Request-User": "alice"}
	status, body = dataCallWithHeaders(t, dataPlane.URL, http.MethodGet, "/files/window", nil, "", "", forged)
	if status != http.StatusUnauthorized {
		t.Fatalf("open the window with a forwarded user = %d %s, want 401", status, body)
	}

	// A Guest reads but never writes (C-59): upload, rename and delete are
	// refused.
	guest := createAccount(t, shell, ownerToken, "guest", "guest-password", write.RoleGuest)
	status, body = dataCall(t, dataPlane.URL, http.MethodPut,
		"/files/"+harness.Namespace+"/alice-docs/content?path=notes.txt", []byte("pwned"), "application/octet-stream", guest)
	if status != http.StatusForbidden {
		t.Fatalf("a guest upload = %d %s, want 403", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodPost, "/files/"+harness.Namespace+"/alice-docs/rename",
		[]byte(`{"from":"notes.txt","to":"moved.txt"}`), "application/json", guest)
	if status != http.StatusForbidden {
		t.Fatalf("a guest rename = %d %s, want 403", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodDelete,
		"/files/"+harness.Namespace+"/alice-docs/content?path=notes.txt", nil, "", guest)
	if status != http.StatusForbidden {
		t.Fatalf("a guest delete = %d %s, want 403", status, body)
	}
	if content, err := os.ReadFile(filepath.Join(aliceRoot, "notes.txt")); err != nil || string(content) != "hello" {
		t.Fatalf("a guest changed the file: %q, %v", content, err)
	}

	// The control plane offers no file interface: the data plane is the
	// product's own service (NG10).
	status, _ = shell.Call(t, http.MethodGet, "/files/window", nil, alice)
	if status != http.StatusNotFound {
		t.Fatalf("the control plane answered the data-plane path with %d, want 404", status)
	}
	if len(filemanager.Declaration().Contributions.Pages) != 0 {
		t.Fatal("the file manager declares a panel page; its interface is an application window")
	}

	// Removing the product withdraws the setting and the preference schema.
	cluster.DeleteComponent(t, filemanagerComponent)
	registry := cluster.ExtensionState(t, artifacts).Registry()
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); found {
		t.Fatal("the exposed-volume setting survived the uninstall")
	}
	if _, found := registry.PreferenceKeys(filemanager.Identifier); found {
		t.Fatal("the file manager preference schema survived the uninstall")
	}
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

// writeSetting stores one setting value through the control-plane API.
func writeSetting(t *testing.T, shell *harness.Shell, token, id, value string) {
	t.Helper()
	status, body := shell.Call(t, http.MethodPut, "/api/v1/settings/"+id, map[string]any{"value": value}, token)
	if status != http.StatusOK {
		t.Fatalf("write setting %s = %d %v", id, status, body)
	}
}

// writePreference stores one preference value for the calling account.
func writePreference(t *testing.T, shell *harness.Shell, token, key, value string) {
	t.Helper()
	status, body := shell.Call(t, http.MethodPut,
		"/api/v1/preferences/"+filemanager.Identifier+"/"+key, map[string]any{"value": value}, token)
	if status != http.StatusNoContent {
		t.Fatalf("write preference %s = %d %v", key, status, body)
	}
}

// createAccount creates one account through the control-plane API and returns a
// token issued after its one-time password was replaced.
func createAccount(t *testing.T, shell *harness.Shell, ownerToken, username, password string, role write.Role) string {
	t.Helper()
	status, body := shell.Call(t, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": username, "role": string(role),
	}, ownerToken)
	if status != http.StatusCreated {
		t.Fatalf("create account %s = %d %v", username, status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	if oneTime == "" {
		t.Fatalf("create account %s returned no one-time password: %v", username, body)
	}
	token, _ := shell.Accounts.SignIn(t, username, oneTime)
	shell.Accounts.ChangePassword(t, token, oneTime, password)
	token, _ = shell.Accounts.SignIn(t, username, password)
	return token
}

// dataCall sends one request to the product's data plane and returns the status
// and the raw body.
func dataCall(t *testing.T, base, method, path string, body []byte, contentType, token string) (int, []byte) {
	t.Helper()
	return dataCallWithHeaders(t, base, method, path, body, contentType, token, nil)
}

// downloadHeader sends one download and returns the Content-Disposition header
// with the body, so a case can read how the data plane presents a file name.
func downloadHeader(t *testing.T, base, token, path string) (string, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("build the download: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("download %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the download: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download %s = %d %s", path, response.StatusCode, body)
	}
	return response.Header.Get("Content-Disposition"), body
}

// dataCallWithHeaders sends one request with extra headers, so a case can
// present a forwarded user the data plane must not trust (C-46).
func dataCallWithHeaders(t *testing.T, base, method, path string, body []byte, contentType, token string, headers map[string]string) (int, []byte) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, base+path, payload)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response.StatusCode, answer
}

// decodeWindow reads the window response.
func decodeWindow(t *testing.T, body []byte) filemanager.Window {
	t.Helper()
	var window filemanager.Window
	if err := json.Unmarshal(body, &window); err != nil {
		t.Fatalf("decode the window %q: %v", body, err)
	}
	return window
}
