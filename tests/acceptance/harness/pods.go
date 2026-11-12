package harness

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// PodFixture is one Pod an application's workload owns, shaped with the status
// the runtime projection reads (C-76). The Pod has one container named "app".
type PodFixture struct {
	// Namespace is where the Pod lives; empty means Namespace.
	Namespace string
	// Name is the Pod's identity.
	Name string
	// Labels must carry the selector of the workload that owns the Pod.
	Labels map[string]string
	// Node is the node the Pod runs on; empty leaves it unscheduled.
	Node string
	// Phase is the runtime phase; empty means Running.
	Phase string
	// Ready sets the Ready condition.
	Ready bool
	// Restarts is the container's restart count.
	Restarts int64
	// Image is the container image; empty means nginx:1.27.
	Image string
	// StartedAt is status.startTime; zero omits it.
	StartedAt time.Time
}

// CreatePod writes one Pod fixture to the cluster and its status, which the
// runtime view reads.
func (c *Cluster) CreatePod(t *testing.T, fixture PodFixture) *unstructured.Unstructured {
	t.Helper()
	namespace := fixture.Namespace
	if namespace == "" {
		namespace = Namespace
	}
	image := fixture.Image
	if image == "" {
		image = "nginx:1.27"
	}
	phase := fixture.Phase
	if phase == "" {
		phase = "Running"
	}
	labels := make(map[string]any, len(fixture.Labels))
	for key, value := range fixture.Labels {
		labels[key] = value
	}
	spec := map[string]any{"containers": []any{map[string]any{"name": "app", "image": image}}}
	if fixture.Node != "" {
		spec["nodeName"] = fixture.Node
	}
	status := map[string]any{
		"phase": phase,
		"conditions": []any{map[string]any{
			"type": "Ready", "status": readyStatus(fixture.Ready),
		}},
		"containerStatuses": []any{map[string]any{
			"name": "app", "image": image, "restartCount": fixture.Restarts,
			"ready": fixture.Ready, "started": true,
		}},
	}
	if !fixture.StartedAt.IsZero() {
		status["startTime"] = fixture.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": fixture.Name, "namespace": namespace, "labels": labels},
		"spec":       spec,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.Dynamic.Resource(schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}).Namespace(namespace)
	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod %s/%s: %v", namespace, fixture.Name, err)
	}
	created.Object["status"] = status
	updated, err := resource.UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("write status of pod %s/%s: %v", namespace, fixture.Name, err)
	}
	return updated
}

// readyStatus renders the Ready condition value for one expected readiness.
func readyStatus(ready bool) string {
	if ready {
		return "True"
	}
	return "False"
}

// Pods lists the Pods stored in one namespace.
func (c *Cluster) Pods(t *testing.T, namespace string) []unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	list, err := c.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list pods in %s: %v", namespace, err)
	}
	return list.Items
}

// RuntimeApplication is a stored application whose status names the Deployments
// its last render wrote, so the runtime view resolves its Pods without an
// install (C-76). spec stays the caller's: ownership, visibility, volumes and
// replica count are what each case varies.
func RuntimeApplication(name string, spec map[string]any, deployments ...string) map[string]any {
	rendered := make([]any, 0, len(deployments))
	for _, deployment := range deployments {
		rendered = append(rendered, map[string]any{
			"group": "apps", "version": "v1", "kind": "Deployment",
			"namespace": Namespace, "name": deployment,
		})
	}
	if spec == nil {
		spec = map[string]any{}
	}
	if _, set := spec["namespace"]; !set {
		spec["namespace"] = Namespace
	}
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": Namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec":   spec,
		"status": map[string]any{"rendered": rendered},
	}
}

// RuntimeDeployment is one workload an application renders: it selects the Pods
// that carry app=<name>, so the runtime view finds exactly this application's
// Pods in a shared namespace.
func RuntimeDeployment(name string, replicas int64) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": Namespace},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": name}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "app", "image": "nginx:1.27",
				}}},
			},
		},
	}
}
