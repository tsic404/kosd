package install

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

// TestDeployManifestMatchesRenderer pins the shipped deployment path to the
// installation renderer: applying config/deploy/kos-control-plane.yaml must put
// the same control-plane objects in the cluster an installation writes
// (KOSD-AC-73, ADM-C-54). An operator reads one workload, not two shapes.
func TestDeployManifestMatchesRenderer(t *testing.T) {
	manifests := readDeployManifest(t, "../../config/deploy/kos-control-plane.yaml")
	documents, err := Render(Plan{Mode: ModeExisting, Owner: "owner"})
	if err != nil {
		t.Fatalf("render an existing-cluster plan: %v", err)
	}
	if len(manifests) != len(documents) {
		t.Fatalf("the shipped manifest carries %d objects, the renderer produces %d", len(manifests), len(documents))
	}
	for _, document := range documents {
		key := objectKey(document.Object.Object)
		shipped, found := manifests[key]
		if !found {
			t.Fatalf("the shipped manifest does not carry %s", key)
		}
		rendered, err := json.Marshal(document.Object.Object)
		if err != nil {
			t.Fatalf("encode %s: %v", key, err)
		}
		encoded, err := json.Marshal(shipped)
		if err != nil {
			t.Fatalf("encode the shipped %s: %v", key, err)
		}
		if !bytes.Equal(rendered, encoded) {
			t.Fatalf("%s differs from the renderer:\nrendered: %s\nshipped:  %s", key, rendered, encoded)
		}
	}

	deployment := manifests["apps/v1/Deployment/kos-control-plane"]
	spec, _ := deployment["spec"].(map[string]any)
	if image, _ := containerField(spec, "image").(string); image != DefaultImage {
		t.Fatalf("the shipped Deployment runs %q, the installation renders %q", image, DefaultImage)
	}
	if replicas, _ := spec["replicas"].(float64); replicas != 1 {
		t.Fatalf("the shipped Deployment runs %v replicas, an installation promises one", replicas)
	}
	strategy, _ := spec["strategy"].(map[string]any)
	if kind, _ := strategy["type"].(string); kind != "Recreate" {
		t.Fatalf("the shipped Deployment uses the %q strategy, an installation renders Recreate", kind)
	}
}

// TestControlPlaneDeploymentCarriesThePackageVolume pins the deployable delivery
// channel: every install form mounts the host directory an operator drops
// packages in and tells the process to read it (KOSD-C-17, KOSD-NG-7).
func TestControlPlaneDeploymentCarriesThePackageVolume(t *testing.T) {
	plans := map[string]Plan{
		"existing-cluster": {Mode: ModeExisting, Owner: "owner"},
		"external-control-plane": {
			Mode: ModeExternal, Owner: "owner",
			Managed: ManagedEndpoint{Server: "https://managed.example"},
		},
	}
	for name, plan := range plans {
		normalized, err := plan.Normalize()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		deployment := controlPlaneDeployment(normalized, map[string]any{})
		containers, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "containers")
		if err != nil || len(containers) != 1 {
			t.Fatalf("%s: containers = %v (%v), want one", name, containers, err)
		}
		container, _ := containers[0].(map[string]any)
		if args, _ := container["args"].([]any); len(args) != 2 ||
			args[0] != "-packages" || args[1] != PackageDirectory {
			t.Fatalf("%s: args = %v, want -packages %s", name, container["args"], PackageDirectory)
		}
		mounts, _ := container["volumeMounts"].([]any)
		if !hasPackageMount(mounts) {
			t.Fatalf("%s: volumeMounts = %v, want the package directory read-only", name, mounts)
		}
		volumes, _, err := unstructured.NestedSlice(deployment.Object, "spec", "template", "spec", "volumes")
		if err != nil || !hasPackageVolume(volumes) {
			t.Fatalf("%s: volumes = %v (%v), want the host package directory", name, volumes, err)
		}
	}
}

// hasPackageMount reports whether one mount carries the package directory
// read-only.
func hasPackageMount(mounts []any) bool {
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		if mount["name"] == PackageVolume && mount["mountPath"] == PackageDirectory && mount["readOnly"] == true {
			return true
		}
	}
	return false
}

// hasPackageVolume reports whether one volume is the host package directory.
func hasPackageVolume(volumes []any) bool {
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		hostPath, _ := volume["hostPath"].(map[string]any)
		if volume["name"] == PackageVolume && hostPath["path"] == PackageDirectory && hostPath["type"] == "DirectoryOrCreate" {
			return true
		}
	}
	return false
}

// containerField reads one field of the first container, which is where an
// operator looks for the image a workload runs.
func containerField(spec map[string]any, field string) any {
	containers, _ := spec["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	if len(containers) == 0 {
		return nil
	}
	container, _ := containers[0].(map[string]any)
	return container[field]
}

// readDeployManifest decodes every document of the shipped manifest, indexed by
// its kind, version and name.
func readDeployManifest(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped manifest: %v", err)
	}
	manifests := map[string]map[string]any{}
	for _, document := range strings.Split(string(content), "\n---\n") {
		object := map[string]any{}
		if err := yaml.Unmarshal([]byte(document), &object); err != nil {
			t.Fatalf("decode a shipped document: %v", err)
		}
		if len(object) == 0 {
			continue
		}
		manifests[objectKey(object)] = object
	}
	return manifests
}

// objectKey names one object the way a reader compares it across the two
// sources.
func objectKey(object map[string]any) string {
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	apiVersion, _ := object["apiVersion"].(string)
	kind, _ := object["kind"].(string)
	return fmt.Sprintf("%s/%s/%s", apiVersion, kind, name)
}
