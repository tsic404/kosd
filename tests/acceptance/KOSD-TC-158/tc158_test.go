// Package kosdtc158 checks KOSD-AC-73's delivery split and device-fact write
// scope on one multi-node installation: this repository delivers exactly the
// control-plane artifact, the run observes every declared node, and each node's
// agent identity writes only its own device fact (KOSD-C-12, ADM-C-54).
//
// ADM-C-54's other two artifacts are asserted by the kosadm ADM-TC-131 carrier
// (the per-node agent DaemonSet and the management CLI) and the agent-side
// hostname boundary by the kos-node NODE-TC-81 carrier. Those repositories
// import this one, so this case cannot import them; it asserts the kernel half
// of the same installation.
package kosdtc158

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/device"
	"github.com/tsic404/kosd/pkg/install"
	"github.com/tsic404/kosd/pkg/markers"
	"github.com/tsic404/kosd/pkg/write"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

// nodeA, nodeB and nodeC are the nodes the bootstrap installation declares.
const (
	nodeA = "node-a"
	nodeB = "node-b"
	nodeC = "node-c"
)

// declaredNodes is the bootstrap plan's node count.
const declaredNodes = 3

// TestTC158MultiNodeInstallAndAgentWriteScope runs one three-node bootstrap
// installation, reads what it delivered, then writes one device fact per node
// through the agent identity of that same installation.
func TestTC158MultiNodeInstallAndAgentWriteScope(t *testing.T) {
	cluster := harness.Start(t)
	installerIdentity := harness.InstallerIdentity()
	cluster.GrantClusterAdmin(t, installerIdentity)
	accounts := harness.OpenIdentity(t, nil, nil)
	clients := cluster.InstallClients(t, installerIdentity)
	ctx := context.Background()

	provisioner := &nodeProvisioner{client: cluster.Kube, names: []string{nodeA, nodeB, nodeC}}
	runner, err := install.New(install.Config{
		Plan:          harness.BootstrapPlan(declaredNodes, nil),
		Dynamic:       clients.Dynamic,
		Kube:          clients.Kube,
		APIExtensions: clients.APIExtensions,
		Definitions:   harness.Definitions(t),
		Accounts:      accounts.Service,
		Provisioner:   provisioner,
		Nodes:         nodeSource{cluster: cluster},
		QuietWindow:   200 * time.Millisecond,
		SettleTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the bootstrap installation: %v", err)
	}
	result, err := runner.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("run the bootstrap installation: %v", err)
	}
	if result.StoppedAt != "" {
		t.Fatalf("the installation stopped at step %q", result.StoppedAt)
	}
	// The run observed every declared node, which is the node set the per-node
	// agent is deployed against.
	if result.Nodes != declaredNodes {
		t.Fatalf("the installation observed %d nodes, want the declared %d", result.Nodes, declaredNodes)
	}

	t.Run("delivers the control-plane artifact only", func(t *testing.T) {
		requireControlPlaneArtifact(t, cluster)
	})
	t.Run("each node agent writes only its own fact", func(t *testing.T) {
		requirePerNodeWriteScope(t, cluster)
	})
}

// requireControlPlaneArtifact checks the artifact this repository delivers: one
// control-plane workload and its service, the identities and definitions, and
// no per-node workload folded into it.
func requireControlPlaneArtifact(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := cluster.Kube.CoreV1().Namespaces().Get(ctx, install.DefaultNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("the installation left no namespace: %v", err)
	}
	if _, err := cluster.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx,
		"applications."+api.Group, metav1.GetOptions{}); err != nil {
		t.Fatalf("the installation left no object definitions: %v", err)
	}
	controlPlane, err := cluster.Kube.AppsV1().Deployments(install.DefaultNamespace).Get(ctx, install.ControlPlaneName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the installation delivered no control plane: %v", err)
	}
	if replicas := *controlPlane.Spec.Replicas; replicas != 1 {
		t.Fatalf("control-plane replicas = %d, want one", replicas)
	}
	containers := controlPlane.Spec.Template.Spec.Containers
	if len(containers) != 1 || containers[0].Image != install.DefaultImage {
		t.Fatalf("the control-plane workload runs %v, want only %s", containers, install.DefaultImage)
	}
	if got := controlPlane.Spec.Template.Spec.ServiceAccountName; got != install.ControlPlaneServiceAccount {
		t.Fatalf("control-plane identity = %q, want %q", got, install.ControlPlaneServiceAccount)
	}
	if _, err := cluster.Kube.CoreV1().Services(install.DefaultNamespace).Get(ctx, install.ControlPlaneName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the installation delivered no control-plane service: %v", err)
	}

	// The per-node agent is not folded into this artifact: an installation
	// that started carrying it here would break the three-artifact split.
	daemons, err := cluster.Kube.AppsV1().DaemonSets(install.DefaultNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("read the per-node workloads: %v", err)
	}
	if len(daemons.Items) != 0 {
		t.Fatalf("the control-plane installation delivered %d DaemonSets; the node agent is a separate artifact", len(daemons.Items))
	}
	states, err := cluster.Kube.AppsV1().StatefulSets(install.DefaultNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("read the stateful workloads: %v", err)
	}
	if len(states.Items) != 0 {
		t.Fatalf("the control-plane installation delivered %d StatefulSets, want none", len(states.Items))
	}

	// The management CLI is not built here either: this repository's delivery
	// entry points name the control plane only.
	entries, err := os.ReadDir(harness.RepoRoot(t) + "/cmd")
	if err != nil {
		t.Fatalf("read the delivery entry points: %v", err)
	}
	commands := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			commands = append(commands, entry.Name())
		}
	}
	sort.Strings(commands)
	if want := []string{"kos-control-plane"}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("delivered commands = %v, want only the control plane %v", commands, want)
	}
}

// requirePerNodeWriteScope writes one fact per installed node through the agent
// identity, then checks the kernel rules that make "one agent per node, only
// its own fact" enforceable on the stored objects.
func requirePerNodeWriteScope(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	installed := installedNodes(t, cluster)
	if len(installed) != declaredNodes {
		t.Fatalf("the installation left %d nodes, want %d", len(installed), declaredNodes)
	}
	writer := cluster.Writer()
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)

	// Each node's agent writes the fact named after its node.
	for _, node := range installed {
		if _, err := writer.Create(ctx, write.SubjectDiscovery, write.Session{}, deviceFact(node, node, now)); err != nil {
			t.Fatalf("agent for %s writes its own fact: %v", node, err)
		}
	}
	facts := cluster.List(t, "v1beta1", "NodeDevice")
	if names := factNames(facts); !reflect.DeepEqual(names, installed) {
		t.Fatalf("device facts = %v, want exactly one per installed node %v", names, installed)
	}
	for _, fact := range facts {
		if got, _, _ := unstructured.NestedString(fact.Object, "spec", "node"); got != fact.GetName() {
			t.Fatalf("fact %s names node %q, want its own name", fact.GetName(), got)
		}
	}

	// One object per node: an agent cannot leave a second fact behind.
	if _, err := writer.Create(ctx, write.SubjectDiscovery, write.Session{}, deviceFact(nodeA, nodeA, now)); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("a second fact for %s = %v, want the one object per node", nodeA, err)
	}

	// An agent writing another node's object under a different name is stored
	// and marked invalid rather than trusted, and the mark is the validator's.
	impersonated, err := writer.Create(ctx, write.SubjectDiscovery, write.Session{}, deviceFact("intruder", nodeA, now))
	if err != nil {
		t.Fatalf("write a fact naming another node: %v", err)
	}
	validator := device.Validator{Writer: writer}
	violated, err := validator.CheckBoundary(ctx, impersonated)
	if err != nil {
		t.Fatalf("check the fact boundary: %v", err)
	}
	if !violated {
		t.Fatal("a fact naming another node passed the boundary check")
	}
	stored := cluster.Get(t, "v1beta1", "NodeDevice", "intruder")
	if got := stored.GetLabels()[markers.Invalid]; got == "" {
		t.Fatalf("the out-of-boundary fact carries labels %v, want the invalid marker", stored.GetLabels())
	}
	var typed v1beta1.NodeDevice
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, &typed); err != nil {
		t.Fatalf("read the stored fact: %v", err)
	}
	invalid, _ := device.FactMarkers(stored, &typed, now)
	if !invalid {
		t.Fatal("the device view presents the out-of-boundary fact as trusted")
	}
	if _, err := writer.MarkOn(ctx, write.SubjectDiscovery, write.Session{}, stored,
		map[string]string{markers.Invalid: "true"}, nil); err == nil {
		t.Fatal("a node agent wrote the validity marker")
	}
	maintainer := &device.Maintainer{Writer: writer}
	if err := maintainer.Run(ctx); err != nil {
		t.Fatalf("run the device maintenance round: %v", err)
	}
	if got := cluster.Get(t, "v1beta1", "NodeDevice", "intruder").GetLabels()[markers.Invalid]; got == "" {
		t.Fatal("the invalid marker was cleared by a maintenance round")
	}

	// The agent writes nothing else: another kernel kind, a status, a delete
	// outside its own class and a user write are all refused.
	var denied *write.Denied
	if _, err := writer.Create(ctx, write.SubjectDiscovery, write.Session{}, volume()); !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a node agent creates a Volume = %v, want a 403 denial", err)
	}
	if _, err := writer.Apply(ctx, write.SubjectDiscovery, write.Session{}, write.PartStatus, deviceStatus(nodeA)); !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a node agent writes the fact status = %v, want a 403 denial", err)
	}
	cluster.Create(t, harness.Volume(harness.Namespace, "data", map[string]any{
		"mobility":     "cluster-movable",
		"capacity":     "1Gi",
		"storageClass": "standard",
	}))
	if err := writer.Delete(ctx, write.SubjectDiscovery, write.Session{}, write.RefOf(
		cluster.Get(t, "v1beta1", "Volume", "data"))); !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a node agent deletes a Volume = %v, want a 403 denial", err)
	}
	owner := write.Session{Username: "owner", Role: write.RoleOwner}
	if _, err := writer.Create(ctx, write.SubjectUser, owner, deviceFact(nodeB, nodeB, now)); !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a user creates a device fact = %v, want a 403 denial", err)
	}
	if _, err := writer.Apply(ctx, write.SubjectUser, owner, write.PartSpec, deviceFact(nodeB, nodeB, now)); !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a user rewrites a device fact = %v, want a 403 denial", err)
	}

	// The refused writes changed nothing: the per-node facts plus the one
	// out-of-boundary object and the volume are the only stored objects.
	if names := factNames(cluster.List(t, "v1beta1", "NodeDevice")); len(names) != declaredNodes+1 {
		t.Fatalf("the cluster holds %d device facts, want %d", len(names), declaredNodes+1)
	}
	if got := cluster.Get(t, "v1beta1", "Volume", "data"); got.GetName() != "data" {
		t.Fatal("the refused delete removed the volume")
	}
}

// nodeProvisioner brings the declared nodes up, which is what a bootstrap
// installation counts.
type nodeProvisioner struct {
	client kubernetes.Interface
	names  []string
}

// Provision creates one Ready node object per declared node.
func (p *nodeProvisioner) Provision(ctx context.Context, nodes int) error {
	if nodes != len(p.names) {
		return errors.New("the provisioner carries three node names")
	}
	for _, name := range p.names {
		node, err := p.client.CoreV1().Nodes().Create(ctx, &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		}, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		if _, err := p.client.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// nodeSource reports the run kernel's joined nodes.
type nodeSource struct {
	cluster *harness.Cluster
}

// ReadyNodes lists the nodes that carry a Ready condition.
func (s nodeSource) ReadyNodes(ctx context.Context) ([]string, error) {
	nodes, err := s.cluster.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes.Items))
	for index := range nodes.Items {
		if nodeReady(&nodes.Items[index]) {
			names = append(names, nodes.Items[index].Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// nodeReady reports whether one node advertises the Ready condition.
func nodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// installedNodes lists the node names the installation left behind.
func installedNodes(t *testing.T, cluster *harness.Cluster) []string {
	t.Helper()
	nodes, err := cluster.Kube.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list the installed nodes: %v", err)
	}
	names := make([]string, 0, len(nodes.Items))
	for index := range nodes.Items {
		names = append(names, nodes.Items[index].Name)
	}
	sort.Strings(names)
	return names
}

// deviceFact is one node agent's fact: the object is named after its node and
// the node field names that same node.
func deviceFact(name, node string, now time.Time) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "NodeDevice",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{markers.ManagedBy: "discovery"},
		},
		"spec": map[string]any{
			"node":       node,
			"collector":  node,
			"observedAt": now.UTC().Format(time.RFC3339),
			"devices": []any{map[string]any{
				"name": "nvme0", "serial": "S1", "model": "Acceptance", "kind": "disk",
				"present": true, "recognized": true, "driverReady": true, "published": true,
				"observedAt": now.UTC().Format(time.RFC3339),
			}},
		},
	}}
}

// deviceStatus is a status write on a device fact; no subject owns that status
// (KOSD-C-07).
func deviceStatus(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "NodeDevice",
		"metadata":   map[string]any{"name": name},
		"status":     map[string]any{"phase": "Running"},
	}}
}

// volume is another kernel kind, which the node agent does not write.
func volume() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: harness.Manifest("Volume", harness.Namespace, "agent-volume",
		map[string]string{markers.ManagedBy: "kosd"}, nil)}
}

// factNames lists the stored device fact names in a stable order.
func factNames(facts []*unstructured.Unstructured) []string {
	names := make([]string, 0, len(facts))
	for _, fact := range facts {
		names = append(names, fact.GetName())
	}
	sort.Strings(names)
	return names
}
