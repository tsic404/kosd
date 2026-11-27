// Package kosdtc189 verifies C-19's uninstall half: while a served extension
// object declares a reference to an application, the uninstall stays blocked
// and lists that object, and removing it lets the uninstall finish
// (KOSD-AC-53, KOSD-TC-189).
package kosdtc189

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
	"github.com/tsic404/kosd/pkg/state"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	entryGroup      = "desktop.example.test"
	entryVersion    = "v1"
	entryKind       = "DesktopEntry"
	entryPlural     = "desktopentries"
	entryNamespace  = "desktop"
	entryName       = "shop"
	targetNamespace = "apps"
	applicationName = "shop"
	definitionRef   = "plain"
	// requestTimeout bounds every direct cluster call of the case.
	requestTimeout = 30 * time.Second
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

// TestTC189UninstallBlockedByServedReference runs one uninstall while a served
// extension object names the application, then removes that object.
func TestTC189UninstallBlockedByServedReference(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, entryNamespace)
	// The uninstall gate takes its exclusion record in the system namespace the
	// installation creates; a cluster without it has no gate to hold.
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
	cluster.CreateApplication(t, applicationName, definitionRef, targetNamespace)
	if phase := cluster.SettleApplication(t, controller, applicationName, 6); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q before the delete", phase, app.PhaseReady)
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	// The reference object is created through the control plane: that API is
	// the write path C-19's new-reference rule judges.
	controlPlaneEntry := map[string]any{
		"apiVersion": entryGroup + "/" + entryVersion,
		"kind":       entryKind,
		"metadata": map[string]any{
			"name":      entryName,
			"namespace": entryNamespace,
			"labels":    map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{"applicationRef": map[string]any{
			"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
			"kind":       "Application",
			"namespace":  harness.Namespace,
			"name":       applicationName,
		}},
	}
	status, body := gateway.Call(t, http.MethodPost,
		"/api/v1/resources/DesktopEntry?apiVersion=desktop.example.test/v1&namespace=desktop",
		controlPlaneEntry, nil)
	if status != http.StatusAccepted {
		t.Fatalf("create the desktop entry through the control plane: status %d, body %v", status, body)
	}

	if err := cluster.Direct(t, api.Group, v1beta1.GroupVersion.Version, "Application", harness.Namespace).Delete(
		ctx, applicationName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the application: %v", err)
	}
	cluster.ApplicationRound(t, controller, applicationName)

	// The application stays observable and names the uninstall as uninstalling:
	// a direct uninstall would already have removed it here.
	stored := cluster.Get(t, v1beta1.GroupVersion.Version, "Application", applicationName)
	if phase := cluster.ApplicationPhase(t, applicationName); phase != app.PhaseUninstalling {
		t.Fatalf("phase = %q, want %q after the delete", phase, app.PhaseUninstalling)
	}
	if !hasFinalizer(stored, app.Finalizer) {
		t.Fatal("the uninstall gate was released while the reference still exists")
	}
	uninstallStatus := cluster.ApplicationStatusOf(t, applicationName)
	condition, blockedCondition := state.Find(uninstallStatus.Conditions, app.ConditionUninstallBlocked)
	if !blockedCondition || condition.Status != metav1.ConditionTrue {
		t.Fatalf("UninstallBlocked = %+v, want True: %+v", condition, uninstallStatus.Conditions)
	}
	if uninstallStatus.Uninstall == nil || len(uninstallStatus.Uninstall.Blocked) == 0 {
		t.Fatalf("status.uninstall.blocked is empty: %+v", uninstallStatus.Uninstall)
	}
	if !sameRefs(uninstallStatus.Uninstall.Blocked, []v1beta1.ObjectRef{{
		Group: entryGroup, Version: entryVersion, Kind: entryKind,
		Namespace: entryNamespace, Name: entryName,
	}}) {
		t.Fatalf("blocked = %v, want exactly the desktop entry", uninstallStatus.Uninstall.Blocked)
	}

	// Removing the reference lets the uninstall finish.
	if err := cluster.ExtensionResource(entryGroup, entryVersion, entryPlural, entryNamespace).Delete(
		ctx, entryName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the desktop entry: %v", err)
	}
	harness.Eventually(t, 15*time.Second, "the application to be gone", func() bool {
		cluster.ApplicationRound(t, controller, applicationName)
		_, err := cluster.Direct(t, api.Group, v1beta1.GroupVersion.Version, "Application", harness.Namespace).Get(
			context.Background(), applicationName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}

// hasFinalizer reports whether the application still carries the uninstall gate.
func hasFinalizer(object *unstructured.Unstructured, finalizer string) bool {
	for _, item := range object.GetFinalizers() {
		if item == finalizer {
			return true
		}
	}
	return false
}

// sameRefs reports whether two blocked lists name the same objects.
func sameRefs(got, want []v1beta1.ObjectRef) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
