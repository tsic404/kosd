// Package kosdtc54 verifies the phase rules of C-33 against the process the
// deployment manifests start, not against reconcile loops a fixture wires by
// hand: the catalogue sync writes the definition, the application reaches Ready,
// its workload follows the replica, stop and restart contracts, and the other
// kernel subjects have writers as well (KOSD-AC-11, KOSD-AC-27, KOSD-TC-54,
// KOSD-C-03, KOSD-C-04, KOSD-C-33, KOSD-C-66).
package kosdtc54

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
	"github.com/tsic404/kosd/pkg/catalogue"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/pkg/markers"
	"github.com/tsic404/kosd/pkg/volume"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	// deployedApplication is the application the case installs.
	deployedApplication = "web"
	// deployedDefinition is the catalogue entry and definition the package
	// offers.
	deployedDefinition = "web"
	// deployedVolume is the volume that proves the volume subject has a writer.
	deployedVolume = "data"
	// widgetGroup, widgetKind and widgetPlural name the extension class whose
	// declared controller the case installs.
	widgetGroup  = "widgets.example.test"
	widgetKind   = "Widget"
	widgetPlural = "widgets"
)

// deployedWorkloadTemplate renders the Deployment the catalogue entry installs;
// the replicas field comes from the entry's config structure (KOSD-C-32).
const deployedWorkloadTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: {{ .Config.replicas }}
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: {{ .Name }}
          image: example.test/web:1
`

// TestTC54DeployedProcessReachesReady starts the control-plane binary with the
// Deployment manifest's arguments and holds it to the kernel contracts: the
// catalogue sync writes the definition, an application reaches Ready with its
// downstream workload, and the lifecycle actions of C-66 converge it.
func TestTC54DeployedProcessReachesReady(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	// The deployment manifests create the system namespace; envtest starts from
	// a bare API server, so the case applies that one fact itself.
	cluster.EnsureNamespace(t, api.SystemNamespace)
	// The declared controller's class needs its definition installed the way the
	// extension package would install it; the projection, not discovery, tells
	// the process which classes it serves.
	cluster.InstallCRD(t, widgetGroup, "v1", widgetKind, widgetPlural, true)

	process := cluster.StartDeployedProcess(t)
	token := process.SignIn(t)

	// A catalogue source offered by an installed component is what makes a
	// definition exist on a clean cluster (KOSD-C-03 catalogue-sync row,
	// KOSD-C-24, KOSD-AC-11).
	definition := harness.DefinitionContent(t, deployedDefinition, "", deployedWorkloadTemplate, nil, []v1beta1.ConfigField{
		{Name: "replicas", Type: app.TypeInteger, Default: new("1"), Minimum: new("1"), Maximum: new("10")},
	})
	declaration := extension.Declaration{
		Identifier: "builtin.example.test",
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			CatalogueSources: []extension.CatalogueSource{{
				ID:   "builtin",
				Sync: "pull",
				Entries: []extension.CatalogueEntry{{
					ID:         deployedDefinition,
					Display:    extension.CatalogueDisplay{Name: "Web"},
					Definition: definition.Spec,
				}},
			}},
		},
	}
	reference, digest := process.DeliverPackage(t, "builtin-catalogue", declaration)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component("builtin-catalogue", reference, digest)})

	harness.Eventually(t, 120*time.Second, "the deployed process's catalogue sync to write the definition", func() bool {
		stored, err := cluster.Direct(t, api.Group, "v1beta1", "ApplicationDefinition", "").Get(
			context.Background(), deployedDefinition, metav1.GetOptions{})
		if err != nil {
			return false
		}
		return stored.GetLabels()[catalogue.SourceLabel] == "builtin"
	})

	// Condition 1: the process the manifests start renders a Deployment for an
	// application created through its own API (KOSD-C-33, KOSD-AC-27).
	manifest := harness.ApplicationManifest(deployedApplication, harness.Namespace, deployedDefinition,
		targetNamespace, map[string]any{"replicas": int64(2)}, nil)
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	if status, body := process.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, token); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	harness.Eventually(t, 180*time.Second, "the deployed process to drive the application to Ready", func() bool {
		return applicationPhase(t, cluster, deployedApplication) == app.PhaseReady
	})
	assertWorkloadReplicas(t, cluster, int64(2))
	application, err := cluster.GetObject(context.Background(), "v1beta1", "Application", harness.Namespace, deployedApplication)
	if err != nil {
		t.Fatalf("read the application: %v", err)
	}
	if resources, found, _ := unstructured.NestedMap(application.Object, "status", "resources"); !found || len(resources) == 0 {
		t.Fatalf("the ready application carries no status.resources: %v", application.Object["status"])
	}
	if conditions := harness.Conditions(t, application); len(conditions) == 0 {
		t.Fatal("the ready application carries no conditions")
	}

	// Condition 2: stop, start and scale are the spec writes C-66 names, and the
	// workload follows them without the user spec losing its replica count.
	action(t, process, token, app.ActionStop, nil)
	harness.Eventually(t, 120*time.Second, "the stopped application's workload to reach zero replicas", func() bool {
		return workloadReplicas(t, cluster) == 0
	})
	if applicationPhase(t, cluster, deployedApplication) != app.PhaseReady {
		t.Fatalf("phase = %q while stopped, want %q", applicationPhase(t, cluster, deployedApplication), app.PhaseReady)
	}
	if replicas := applicationReplicas(t, cluster); replicas != 2 {
		t.Fatalf("spec.replicas = %d while stopped, want the written 2", replicas)
	}

	action(t, process, token, app.ActionStart, nil)
	harness.Eventually(t, 120*time.Second, "the started application's workload to reach two replicas", func() bool {
		return workloadReplicas(t, cluster) == 2
	})

	action(t, process, token, app.ActionScale, map[string]any{"replicas": 3})
	harness.Eventually(t, 120*time.Second, "the scaled application's workload to reach three replicas", func() bool {
		return workloadReplicas(t, cluster) == 3
	})

	// A restart travels as a one-shot record: the reconciler rebuilds the
	// rendered workload and deletes the record (KOSD-C-04, KOSD-C-66). A rebuild
	// replaces the Deployment object, so the workload's UID must change; an
	// implementation that only consumes the record leaves the UID alone, and one
	// that deletes without rebuilding leaves no workload to read.
	application, err = cluster.GetObject(context.Background(), "v1beta1", "Application", harness.Namespace, deployedApplication)
	if err != nil {
		t.Fatalf("read the application: %v", err)
	}
	beforeRestart := deploymentUID(t, cluster)
	recordName := app.ActionRecordName(string(application.GetUID()), app.ActionRestart)
	accepted := action(t, process, token, app.ActionRestart, nil)
	if ref, _ := accepted["ref"].(map[string]any); harness.String(ref, "name") != recordName {
		t.Fatalf("the restart accepted %v, want the record %s", accepted["ref"], recordName)
	}
	record := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace)
	// The record is consumed on a sweep, so it may already be gone; a read
	// failure other than that still means it was never written.
	if _, err := record.Get(context.Background(), recordName, metav1.GetOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("the restart did not write its action record: %v", err)
	}
	harness.Eventually(t, 120*time.Second, "the reconciler to consume and delete the restart record", func() bool {
		_, err := record.Get(context.Background(), recordName, metav1.GetOptions{})
		if err == nil {
			return false
		}
		if !apierrors.IsNotFound(err) {
			t.Fatalf("read the restart record: %v", err)
		}
		return true
	})
	harness.Eventually(t, 120*time.Second, "the restarted application's workload to be rebuilt", func() bool {
		rebuilt := deploymentUID(t, cluster)
		return rebuilt != "" && rebuilt != beforeRestart
	})
	rebuildUID := deploymentUID(t, cluster)
	assertWorkloadReplicas(t, cluster, 3)
	// Only one rebuild: a second round that rebuilt the workload would replace
	// the object again, so the UID has to stay put while the loops keep running.
	stableUntil := time.Now().Add(15 * time.Second)
	for time.Now().Before(stableUntil) {
		if current := deploymentUID(t, cluster); current != rebuildUID {
			t.Fatalf("the restart rebuilt the workload more than once: UID %s -> %s", rebuildUID, current)
		}
		assertWorkloadReplicas(t, cluster, 3)
		time.Sleep(200 * time.Millisecond)
	}

	// A volume is another kernel subject: its reconciler creates the volume's
	// claim and writes its phase, which no fixture did in this process
	// (KOSD-C-03 volume row, KOSD-C-38).
	volumeManifest := harness.Manifest("Volume", harness.Namespace, deployedVolume, map[string]string{
		markers.ManagedBy: "kosd",
	}, nil)
	volumeManifest["spec"] = map[string]any{
		"mobility": string(v1beta1.VolumeMobilityNodeLocal),
		"capacity": "1Gi",
	}
	if status, body := process.Call(t, http.MethodPost, "/api/v1/resources/Volume", volumeManifest, token); status != http.StatusAccepted {
		t.Fatalf("create volume: status %d, body %v", status, body)
	}
	harness.Eventually(t, 120*time.Second, "the deployed process to write the volume's phase", func() bool {
		stored, err := cluster.GetObject(context.Background(), "v1beta1", "Volume", harness.Namespace, deployedVolume)
		if err != nil {
			return false
		}
		phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
		return phase == volume.PhasePending || phase == volume.PhaseBound
	})
	if _, err := cluster.Direct(t, "", "v1", "PersistentVolumeClaim", harness.Namespace).Get(
		context.Background(), deployedVolume, metav1.GetOptions{}); err != nil {
		t.Fatalf("the volume reconciler did not create the claim: %v", err)
	}

	// C-43/C-44: deleting a settled endpoint releases its route and ledger key.
	// The process has no watch source, so the sweep has to deliver the object
	// that disappeared for the reconciler's NotFound branch to run at all.
	endpointName := "web-route"
	endpointHost := "web.example.test"
	endpointSpec := map[string]any{"kind": "http.host", "targetApp": deployedApplication, "host": endpointHost}
	createEndpoint(t, process, token, endpointName, endpointSpec)
	harness.Eventually(t, 120*time.Second, "the endpoint reconciler to write the route", func() bool {
		return harness.IngressOf(t, cluster, endpointName) != nil
	})
	deleteEndpoint(t, process, cluster, token, endpointName)
	harness.Eventually(t, 120*time.Second, "the deleted endpoint's route to be removed", func() bool {
		return harness.IngressOf(t, cluster, endpointName) == nil
	})
	// The ledger released the host key: a different endpoint may take it, and is
	// admitted rather than flagged as a duplicate. An implementation that keeps
	// the deleted entry leaves this one conflicted and without a route (C-44).
	successorName := endpointName + "-next"
	createEndpoint(t, process, token, successorName, endpointSpec)
	harness.Eventually(t, 120*time.Second, "a new endpoint to take over the released host", func() bool {
		return harness.IngressOf(t, cluster, successorName) != nil
	})

	// An installed component's declared controller reaches the process's
	// controller set. This binary links no implementation, so the class is
	// served but its controller is unavailable: the object projects Unknown
	// instead of the stored status nobody maintains (KOSD-C-08, KOSD-C-20).
	widgetReference, widgetDigest := process.DeliverPackage(t, "builtin-controller", widgetDeclaration())
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component("builtin-controller", widgetReference, widgetDigest)})
	harness.Eventually(t, 120*time.Second, "the declared controller's component to become Ready", func() bool {
		return componentPhase(t, cluster, "builtin-controller") == extension.PhaseReady
	})
	createWidget(t, cluster)
	harness.Eventually(t, 120*time.Second, "the declared controller's class to project Unknown", func() bool {
		status, body := process.Call(t, http.MethodGet,
			"/api/v1/resources/Widget/widget-one?apiVersion="+widgetGroup+"/v1&namespace="+targetNamespace, nil, token)
		if status != http.StatusOK {
			return false
		}
		return objectHealth(body) == "Unknown" && objectReconciled(body) == "Unknown"
	})

	// Disabling the component withdraws the class and stops its controllers
	// (KOSD-C-28); the object itself stays (KOSD-C-29).
	cluster.SetComponentDisabled(t, "builtin-controller", true)
	harness.Eventually(t, 120*time.Second, "the disabled component's class to stop being served", func() bool {
		status, _ := process.Call(t, http.MethodGet,
			"/api/v1/resources/Widget/widget-one?apiVersion="+widgetGroup+"/v1&namespace="+targetNamespace, nil, token)
		return status >= 400
	})
	if _, err := cluster.ExtensionResource(widgetGroup, "v1", widgetPlural, targetNamespace).Get(
		context.Background(), "widget-one", metav1.GetOptions{}); err != nil {
		t.Fatalf("disabling the component removed its object: %v", err)
	}
}

// action submits one lifecycle action, requires its acceptance (KOSD-C-66) and
// returns the answer. The reconciler writes the application too, so the API's
// resourceVersion conflict answer is retried: it is retryable and the actions
// are idempotent (KOSD-C-05).
func action(t *testing.T, process *harness.DeployedProcess, token, name string, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": deployedApplication, "action": name,
	}
	for key, value := range extra {
		body[key] = value
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		status, response := process.Call(t, http.MethodPost, "/api/v1/action-requests", body, token)
		switch status {
		case http.StatusAccepted:
			return response
		case http.StatusConflict:
			if time.Now().After(deadline) {
				t.Fatalf("%s application lost the write race throughout: %v", name, response)
			}
			time.Sleep(20 * time.Millisecond)
		default:
			t.Fatalf("%s application: status %d, body %v", name, status, response)
		}
	}
}

// applicationPhase reads the stored phase of the case's application.
func applicationPhase(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	stored, err := cluster.GetObject(context.Background(), "v1beta1", "Application", harness.Namespace, name)
	if err != nil {
		return ""
	}
	phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
	return phase
}

// applicationReplicas reads the written replica count of the case's
// application.
func applicationReplicas(t *testing.T, cluster *harness.Cluster) int64 {
	t.Helper()
	stored, err := cluster.GetObject(context.Background(), "v1beta1", "Application", harness.Namespace, deployedApplication)
	if err != nil {
		t.Fatalf("read the application: %v", err)
	}
	replicas, _, _ := unstructured.NestedInt64(stored.Object, "spec", "replicas")
	return replicas
}

// workloadReplicas reads the rendered Deployment's replica count; -1 reports a
// workload that is not there.
func workloadReplicas(t *testing.T, cluster *harness.Cluster) int64 {
	t.Helper()
	deployment, err := cluster.Direct(t, "apps", "v1", "Deployment", targetNamespace).Get(
		context.Background(), deployedApplication, metav1.GetOptions{})
	if err != nil {
		return -1
	}
	replicas, found, _ := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
	if !found {
		return 1
	}
	return replicas
}

// deploymentUID reads the rendered Deployment's object identity; an empty value
// reports a workload that is not there.
func deploymentUID(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	deployment, err := cluster.Direct(t, "apps", "v1", "Deployment", targetNamespace).Get(
		context.Background(), deployedApplication, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(deployment.GetUID())
}

// assertWorkloadReplicas requires the rendered Deployment to carry want.
func assertWorkloadReplicas(t *testing.T, cluster *harness.Cluster, want int64) {
	t.Helper()
	if replicas := workloadReplicas(t, cluster); replicas != want {
		t.Fatalf("deployment replicas = %d, want %d", replicas, want)
	}
}

// widgetDeclaration is a package that serves one class with a controller whose
// implementation no binary in this repository links.
func widgetDeclaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   widgetGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: widgetGroup, Kind: widgetKind, Resource: widgetPlural,
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []extension.Controller{{
				ID: "widget", Manages: []extension.GroupKind{{Group: widgetGroup, Kind: widgetKind}},
			}},
		},
	}
}

// componentPhase reads one component's stored phase; an empty value means the
// install pass has not written its status yet.
func componentPhase(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	object, err := cluster.GetObject(context.Background(), "v1beta1", "SystemComponent", "", name)
	if err != nil {
		return ""
	}
	phase, _, _ := unstructured.NestedString(object.Object, "status", "phase")
	return phase
}

// createWidget stores an object of the declared class with a status as if its
// controller had written one, so the projection has something to override.
func createWidget(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgetGroup + "/v1",
		"kind":       widgetKind,
		"metadata": map[string]any{
			"name":      "widget-one",
			"namespace": targetNamespace,
			"labels":    map[string]any{markers.ManagedBy: "acceptance"},
		},
		"status": harness.ObjectStatus(),
	}}
	cluster.CreateClusterObject(t, widgetGroup, "v1", widgetPlural, targetNamespace, object.Object)
}

// createEndpoint submits one endpoint through the process's own API.
func createEndpoint(t *testing.T, process *harness.DeployedProcess, token, name string, spec map[string]any) {
	t.Helper()
	manifest := harness.Manifest("Endpoint", harness.Namespace, name, harness.ManagedLabels(), nil)
	manifest["spec"] = spec
	if status, body := process.Call(t, http.MethodPost, "/api/v1/resources/Endpoint", manifest, token); status != http.StatusAccepted {
		t.Fatalf("create endpoint %s: status %d, body %v", name, status, body)
	}
}

// deleteEndpoint removes one endpoint through the process's own API. A delete
// carries the stored resourceVersion, so the case sends it the way a browser
// does; the reconciler writes status concurrently, so a conflicting read is
// retried with the fresh version rather than failed.
func deleteEndpoint(t *testing.T, process *harness.DeployedProcess, cluster *harness.Cluster, token, name string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		stored, err := cluster.GetObject(context.Background(), "v1beta1", "Endpoint", harness.Namespace, name)
		if err != nil {
			t.Fatalf("read endpoint %s: %v", name, err)
		}
		status, body := process.HTTP.Call(t, http.MethodDelete,
			"/api/v1/resources/Endpoint/"+name+"?namespace="+harness.Namespace, nil,
			map[string]string{"Authorization": "Bearer " + token, "If-Match": stored.GetResourceVersion()})
		if status == http.StatusAccepted {
			return
		}
		if status != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("delete endpoint %s: status %d, body %v", name, status, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// objectHealth reads one projected object's health from an API answer, which
// carries the object under "object".
func objectHealth(answer map[string]any) string {
	return harness.String(answer, "object", "status", "health", "state")
}

// objectReconciled reads one projected object's Reconciled status.
func objectReconciled(answer map[string]any) string {
	object, _ := answer["object"].(map[string]any)
	status, _ := object["status"].(map[string]any)
	conditions, _ := status["conditions"].([]any)
	for _, item := range conditions {
		condition, _ := item.(map[string]any)
		if condition["type"] != api.ConditionReconciled {
			continue
		}
		value, _ := condition["status"].(string)
		return value
	}
	return ""
}
