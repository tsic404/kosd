// Package kosdtc164 verifies the runtime view's permission boundary
// (KOSD-AC-75, KOSD-TC-164): a session that may not see the application reads
// no projection of it, a Guest and a visible Member that does not own the
// application are refused the Pod delete, the Pods stay, and the Owner as well
// as the owning Member are accepted.
package kosdtc164

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// TestTC164UnprivilegedSessionsReadNothingAndDeleteNothing pins the role
// boundary of the projection and of the single Pod delete.
func TestTC164UnprivilegedSessionsReadNothingAndDeleteNothing(t *testing.T) {
	cluster := harness.Start(t)
	visibility := map[string]any{"allowGuests": false, "members": []any{"bob"}}
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("shop", map[string]any{
		"owner": "alice", "visibility": visibility,
	}, "shop")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("shop", 2))
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-0", Labels: map[string]string{"app": "shop"}, Ready: true})
	cluster.CreatePod(t, harness.PodFixture{Name: "shop-1", Labels: map[string]string{"app": "shop"}, Ready: true})
	// A second application of the same namespace, owned by the same Member: the
	// Pod it owns is not reachable through the first application's path.
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: harness.RuntimeApplication("blog", map[string]any{
		"owner": "alice",
	}, "blog")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, harness.RuntimeDeployment("blog", 1))
	cluster.CreatePod(t, harness.PodFixture{Name: "blog-0", Labels: map[string]string{"app": "blog"}, Ready: true})

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	bob := system.Account(t, owner, "bob", "bob-password", write.RoleMember)
	carol := system.Account(t, owner, "carol", "carol-password", write.RoleMember)
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)
	projectionPath := "/api/v1/pods/" + harness.Namespace + "/shop"

	// The unlisted member and the guest read no projection; the listed member
	// and the owner do.
	for _, testCase := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "a member the application does not list", token: carol, want: http.StatusNotFound},
		{name: "a guest the application does not admit", token: guest, want: http.StatusNotFound},
		{name: "a member the application lists", token: bob, want: http.StatusOK},
		{name: "the global owner", token: owner, want: http.StatusOK},
	} {
		t.Run("read "+testCase.name, func(t *testing.T) {
			status, body := system.Call(t, testCase.token, http.MethodGet, projectionPath, nil, nil)
			if status != testCase.want {
				t.Fatalf("read = %d %v, want %d", status, body, testCase.want)
			}
			if testCase.want != http.StatusOK {
				if _, carried := body["items"]; carried {
					t.Fatalf("a hidden application answered with a projection: %v", body)
				}
			}
		})
	}

	// A guest and a visible member that does not own the application are
	// refused the delete, and the Pod is still there.
	for _, testCase := range []struct {
		name  string
		token string
	}{
		{name: "a guest", token: guest},
		{name: "a listed member that does not own the application", token: bob},
		{name: "an unlisted member", token: carol},
	} {
		t.Run("delete by "+testCase.name, func(t *testing.T) {
			status, body := system.Call(t, testCase.token, http.MethodDelete,
				projectionPath+"/shop-0", nil, nil)
			if status != http.StatusForbidden {
				t.Fatalf("delete = %d %v, want 403", status, body)
			}
			if !podExistsIn(t, cluster, harness.Namespace, "shop-0") {
				t.Fatal("a refused delete removed the Pod")
			}
		})
	}

	// The global Owner and the Member owning the application may delete the Pod
	// they name.
	if status, body := system.Call(t, owner, http.MethodDelete, projectionPath+"/shop-0", nil, nil); status != http.StatusAccepted {
		t.Fatalf("owner delete = %d %v, want 202", status, body)
	}
	if podExistsIn(t, cluster, harness.Namespace, "shop-0") {
		t.Fatal("the owner's delete left the Pod in place")
	}
	// The owning Member may delete its own application's Pod, but a Pod of the
	// other application it owns is not reachable through this application's
	// path: the request names a Pod this application does not own.
	if status, body := system.Call(t, alice, http.MethodDelete, projectionPath+"/blog-0", nil, nil); status != http.StatusNotFound {
		t.Fatalf("delete of another application's Pod = %d %v, want 404", status, body)
	}
	if !podExistsIn(t, cluster, harness.Namespace, "blog-0") {
		t.Fatal("the cross-application delete removed the other application's Pod")
	}
	if status, body := system.Call(t, alice, http.MethodDelete, projectionPath+"/shop-1", nil, nil); status != http.StatusAccepted {
		t.Fatalf("owning member delete = %d %v, want 202", status, body)
	}
	if podExistsIn(t, cluster, harness.Namespace, "shop-1") {
		t.Fatal("the owning member's delete left the Pod in place")
	}
}

// podExistsIn reads whether one Pod is still stored in a namespace.
func podExistsIn(t *testing.T, cluster *harness.Cluster, namespace, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Dynamic.Resource(schema.GroupVersionResource{Version: "v1", Resource: "pods"}).
		Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read pod %s/%s: %v", namespace, name, err)
	}
	return true
}

// TestTC164DeleteUsesTheRenderNamespace pins the existence probe of the delete
// to the namespace the application renders into (C-32): where the application
// object and its workloads live in different namespaces, another application's
// Pod is still refused and a Pod that is gone is still an accepted no-op.
func TestTC164DeleteUsesTheRenderNamespace(t *testing.T) {
	const (
		applicationNamespace = "apps"
		renderNamespace      = "workloads"
	)
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, applicationNamespace)
	cluster.EnsureNamespace(t, renderNamespace)
	cluster.CreateIn(t, applicationNamespace, &unstructured.Unstructured{Object: renderedApplication("shop", applicationNamespace, renderNamespace, "alice")})
	cluster.CreateIn(t, applicationNamespace, &unstructured.Unstructured{Object: renderedApplication("blog", applicationNamespace, renderNamespace, "alice")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", renderNamespace, renderedDeployment(renderNamespace, "shop"))
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", renderNamespace, renderedDeployment(renderNamespace, "blog"))
	cluster.CreatePod(t, harness.PodFixture{Namespace: renderNamespace, Name: "shop-0", Labels: map[string]string{"app": "shop"}, Ready: true})
	cluster.CreatePod(t, harness.PodFixture{Namespace: renderNamespace, Name: "blog-0", Labels: map[string]string{"app": "blog"}, Ready: true})

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	podPath := "/api/v1/pods/" + applicationNamespace + "/shop"

	// Another application's Pod in the render namespace is not this
	// application's target: the request is refused and the Pod stays.
	if status, body := system.Call(t, owner, http.MethodDelete, podPath+"/blog-0", nil, nil); status != http.StatusNotFound {
		t.Fatalf("delete of another application's Pod = %d %v, want 404", status, body)
	}
	if !podExistsIn(t, cluster, renderNamespace, "blog-0") {
		t.Fatal("the refused delete removed the other application's Pod")
	}

	// The application's own Pod is deleted from the render namespace.
	if status, body := system.Call(t, owner, http.MethodDelete, podPath+"/shop-0", nil, nil); status != http.StatusAccepted {
		t.Fatalf("delete in the render namespace = %d %v, want 202", status, body)
	}
	if podExistsIn(t, cluster, renderNamespace, "shop-0") {
		t.Fatal("the delete left the Pod in the render namespace")
	}

	// A same-named Pod in the application's own namespace is not the target:
	// a repeat delete is an accepted no-op and leaves that Pod alone.
	cluster.CreatePod(t, harness.PodFixture{Namespace: applicationNamespace, Name: "shop-0", Labels: map[string]string{"app": "decoy"}, Ready: true})
	status, body := system.Call(t, owner, http.MethodDelete, podPath+"/shop-0", nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("repeat delete = %d %v, want 202", status, body)
	}
	if body["idempotent"] != true {
		t.Fatalf("repeat delete idempotent = %v, want true", body["idempotent"])
	}
	if !podExistsIn(t, cluster, applicationNamespace, "shop-0") {
		t.Fatal("the repeat delete removed a same-named Pod outside the render namespace")
	}
}

// renderedApplication is one application object stored in objectNamespace whose
// render lives in renderNamespace (C-32).
func renderedApplication(name, objectNamespace, renderNamespace, owner string) map[string]any {
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": objectNamespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{"namespace": renderNamespace, "owner": owner},
		"status": map[string]any{"rendered": []any{map[string]any{
			"group": "apps", "version": "v1", "kind": "Deployment",
			"namespace": renderNamespace, "name": name,
		}}},
	}
}

// renderedDeployment is one workload in the render namespace, selecting the
// Pods labelled app=<name>.
func renderedDeployment(namespace, name string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
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
