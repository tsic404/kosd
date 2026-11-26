// Package kosdtc190 verifies C-19's new-reference rule: while one application
// uninstalls, the control plane refuses only the writes that add a reference to
// it, leaves a retained reference alone, and still blocks the uninstall on that
// reference (KOSD-AC-53, KOSD-TC-190).
package kosdtc190

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/app"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	entryGroup     = "desktop.example.test"
	entryVersion   = "v1"
	entryKind      = "DesktopEntry"
	entryPlural    = "desktopentries"
	entryNamespace = "desktop"
	// applicationName is the application whose removal is requested; the other
	// application stays live and is the reference an update moves away from.
	applicationName      = "shop"
	otherApplicationName = "billing"
	definitionRef        = "plain"
	entryPath            = "/api/v1/resources/DesktopEntry"
	// requestTimeout bounds every direct cluster call of the case.
	requestTimeout = 30 * time.Second
	// refusedCode is the frozen value of C-19's refusal. It is written out here
	// instead of read from the server package: the case must fail when the
	// server changes the value it answers with.
	refusedCode = "reference_refused"
)

// declaration serves one product class whose objects may reference an
// application.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "thirdparty.desktop",
		Version:    "1.0.0",
		APIGroup:   entryGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: entryGroup, Kind: entryKind, Resource: entryPlural,
				Versions: []string{entryVersion}, Namespaced: true,
				References: []string{"spec.applicationRef"},
			}},
		},
	}
}

// entry renders one desktop entry body naming one application, with the labels
// the write carries.
func entry(name, application string, labels map[string]any) map[string]any {
	return map[string]any{
		"apiVersion": entryGroup + "/" + entryVersion,
		"kind":       entryKind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": entryNamespace,
			"labels":    labels,
		},
		"spec": map[string]any{"applicationRef": map[string]any{
			"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
			"kind":       "Application",
			"namespace":  harness.Namespace,
			"name":       application,
		}},
	}
}

// acceptanceLabels is the label set the writes carry; the label-only update
// adds one more key to it.
func acceptanceLabels() map[string]any {
	return map[string]any{"managed-by": "acceptance"}
}

// referenceOf reads the application one stored entry references.
func referenceOf(t *testing.T, object *unstructured.Unstructured) string {
	t.Helper()
	name, found, err := unstructured.NestedString(object.Object, "spec", "applicationRef", "name")
	if err != nil || !found {
		t.Fatalf("stored entry %s has no application reference: %v", object.GetName(), err)
	}
	return name
}

// refusedByReference asserts one refused write answered 409 with C-19's code.
func refusedByReference(t *testing.T, step string, status int, body map[string]any) {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("%s: status %d, body %v, want 409", step, status, body)
	}
	failure, _ := body["error"].(map[string]any)
	if failure["code"] != refusedCode {
		t.Fatalf("%s: error = %v, want code %q", step, body["error"], refusedCode)
	}
}

// TestTC190NewReferenceRefusedAfterDeleteRequest runs the C-19 write rule end to
// end: one application uninstalls, a create that names it and an update that
// moves a reference onto it are both refused, a label-only update that keeps an
// existing reference passes, and the retained reference then blocks and finally
// releases the uninstall.
func TestTC190NewReferenceRefusedAfterDeleteRequest(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, harness.Namespace)
	cluster.EnsureNamespace(t, entryNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.InstallCRD(t, entryGroup, entryVersion, entryKind, entryPlural, true)
	cluster.InstallDefinition(t, harness.Definition(t, definitionRef, nil))
	installed := cluster.InstallPackage(t, "desktop", declaration(), extension.MemoryArtifacts{})
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Extensions = installed.Registry()
	})

	controller := cluster.ApplicationController(t, app.Options{
		References: extension.ReferenceGate{Dynamic: cluster.Dynamic, Registry: installed.Registry()},
	})
	for _, name := range []string{applicationName, otherApplicationName} {
		cluster.CreateApplication(t, name, definitionRef, harness.Namespace)
		if phase := cluster.SettleApplication(t, controller, name, 6); phase != app.PhaseReady {
			t.Fatalf("%s: phase = %q, want %q before the delete", name, phase, app.PhaseReady)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	entries := cluster.ExtensionResource(entryGroup, entryVersion, entryPlural, entryNamespace)

	// 甲 references the application that will uninstall; 乙 references the one
	// that stays live. Both writes go through the control plane, the path C-19's
	// rule judges.
	entriesToCreate := []struct{ name, application string }{
		{"alpha", applicationName},
		{"beta", otherApplicationName},
	}
	for _, wanted := range entriesToCreate {
		status, body := gateway.Call(t, http.MethodPost,
			entryPath+"?apiVersion="+entryGroup+"/"+entryVersion+"&namespace="+entryNamespace,
			entry(wanted.name, wanted.application, acceptanceLabels()), nil)
		if status != http.StatusAccepted {
			t.Fatalf("create entry %s: status %d, body %v", wanted.name, status, body)
		}
	}

	// The removal request goes through the control plane too, so the case starts
	// from the state the API produces rather than a hand-written one.
	storedApplication := cluster.Get(t, v1beta1.GroupVersion.Version, "Application", applicationName)
	if status, body := gateway.Call(t, http.MethodDelete,
		"/api/v1/resources/Application/"+applicationName+"?namespace="+harness.Namespace, nil,
		map[string]string{"If-Match": storedApplication.GetResourceVersion()}); status != http.StatusAccepted {
		t.Fatalf("delete application %s: status %d, body %v", applicationName, status, body)
	}
	cluster.ApplicationRound(t, controller, applicationName)
	if phase := cluster.ApplicationPhase(t, applicationName); phase != app.PhaseUninstalling {
		t.Fatalf("phase = %q, want %q after the delete", phase, app.PhaseUninstalling)
	}

	// A create that introduces a reference is refused, and nothing of it lands.
	status, body := gateway.Call(t, http.MethodPost,
		entryPath+"?apiVersion="+entryGroup+"/"+entryVersion+"&namespace="+entryNamespace,
		entry("gamma", applicationName, acceptanceLabels()), nil)
	refusedByReference(t, "create entry gamma naming the uninstalling application", status, body)
	objects, err := entries.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list the desktop entries: %v", err)
	}
	for index := range objects.Items {
		if objects.Items[index].GetName() == "gamma" {
			t.Fatalf("the refused entry was stored: %v", objects.Items[index].Object)
		}
	}

	// 乙 starts on the live application. Moving that reference onto the
	// uninstalling one would introduce it, so the update is refused and 乙 is
	// left exactly as it was.
	betaBefore, err := entries.Get(ctx, "beta", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read entry beta: %v", err)
	}
	if referenceOf(t, betaBefore) != otherApplicationName {
		t.Fatalf("entry beta references %q, want %q", referenceOf(t, betaBefore), otherApplicationName)
	}
	status, body = gateway.Call(t, http.MethodPut,
		entryPath+"/beta?apiVersion="+entryGroup+"/"+entryVersion+"&namespace="+entryNamespace,
		entry("beta", applicationName, acceptanceLabels()),
		map[string]string{"If-Match": betaBefore.GetResourceVersion()})
	refusedByReference(t, "update entry beta to reference the uninstalling application", status, body)
	betaAfter, err := entries.Get(ctx, "beta", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reread entry beta: %v", err)
	}
	if referenceOf(t, betaAfter) != otherApplicationName {
		t.Fatalf("the refused update changed entry beta to reference %q, want %q",
			referenceOf(t, betaAfter), otherApplicationName)
	}
	if betaAfter.GetResourceVersion() != betaBefore.GetResourceVersion() {
		t.Fatalf("the refused update rewrote entry beta: resourceVersion %s -> %s",
			betaBefore.GetResourceVersion(), betaAfter.GetResourceVersion())
	}

	// 甲 already references the uninstalling application. Changing only its
	// labels does not introduce a reference, so the update passes and the entry
	// keeps the reference it had.
	alphaBefore, err := entries.Get(ctx, "alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read entry alpha: %v", err)
	}
	alphaLabels := acceptanceLabels()
	alphaLabels["stage"] = "reviewed"
	status, body = gateway.Call(t, http.MethodPut,
		entryPath+"/alpha?apiVersion="+entryGroup+"/"+entryVersion+"&namespace="+entryNamespace,
		entry("alpha", applicationName, alphaLabels),
		map[string]string{"If-Match": alphaBefore.GetResourceVersion()})
	if status != http.StatusAccepted {
		t.Fatalf("label-only update of entry alpha: status %d, body %v, want 202", status, body)
	}
	alphaAfter, err := entries.Get(ctx, "alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reread entry alpha: %v", err)
	}
	if alphaAfter.GetLabels()["stage"] != "reviewed" {
		t.Fatalf("the accepted update did not apply the label: %v", alphaAfter.GetLabels())
	}
	if referenceOf(t, alphaAfter) != applicationName {
		t.Fatalf("entry alpha references %q after the update, want %q",
			referenceOf(t, alphaAfter), applicationName)
	}

	// The retained reference is the only one, so the uninstall stays blocked on
	// exactly that entry.
	if phase := cluster.ApplicationPhase(t, applicationName); phase != app.PhaseUninstalling {
		t.Fatalf("phase = %q, want %q after the writes", phase, app.PhaseUninstalling)
	}
	uninstallStatus := cluster.ApplicationStatusOf(t, applicationName)
	if uninstallStatus.Uninstall == nil || len(uninstallStatus.Uninstall.Blocked) != 1 {
		t.Fatalf("blocked = %+v, want exactly entry alpha", uninstallStatus.Uninstall)
	}
	if blocked := uninstallStatus.Uninstall.Blocked[0]; blocked.Name != "alpha" ||
		blocked.Namespace != entryNamespace || blocked.Kind != entryKind {
		t.Fatalf("blocked[0] = %+v, want entry alpha", blocked)
	}

	// Removing the existing reference lets the uninstall finish.
	if err := entries.Delete(ctx, "alpha", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete entry alpha: %v", err)
	}
	harness.Eventually(t, 15*time.Second, "the application to be gone", func() bool {
		cluster.ApplicationRound(t, controller, applicationName)
		_, err := cluster.Direct(t, api.Group, v1beta1.GroupVersion.Version, "Application", harness.Namespace).Get(
			context.Background(), applicationName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}
