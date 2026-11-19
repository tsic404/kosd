package kosdtc155

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	taskmanagerExtension = "kos-taskmanager"
	taskmanagerComponent = "kos-taskmanager"
	taskmanagerApp       = "kos-taskmanager"
	taskmanagerPage      = "overview"
)

// taskmanagerPackage is the shape a first-party system application carries: one
// page and one window application in the same artifact (KOSD-AC-72).
func taskmanagerPackage() extension.Declaration {
	return extension.Declaration{
		Identifier:       taskmanagerExtension,
		Version:          "0.1.0",
		FrameworkVersion: ">=1.0 <2.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{{
				ID: taskmanagerPage, Title: "概览", Location: "panel",
				ReadOnly: true, ReadOnlyReason: "只读展示",
				Items: []extension.PageItem{{
					ID: "nodes-ready", Label: "节点就绪",
					Source: extension.PageSourceObject, Reference: taskmanagerExtension + "/nodes-ready",
				}},
			}},
			Apps: []extension.App{{
				ID: taskmanagerApp, Name: "运行视图", Version: "0.1.0",
				Icon: "assets/icon.svg", Entry: "https://taskmanager.apps.example/assets/index.html",
				Window: extension.AppWindow{
					Width: 1200, Height: 800, MinWidth: 960, MinHeight: 640,
					Resizable: true, Mode: extension.AppWindowSingle, DefaultState: extension.AppStateNormal,
				},
				Permissions: []string{},
			}},
		},
	}
}

// TestTC155PackageInstallsFromADirectory is the deployable delivery path: the
// artifact is a real file an operator dropped in the package directory, the
// component names its path, and the declared page and window application become
// visible (KOSD-C-17, KOSD-AC-72).
func TestTC155PackageInstallsFromADirectory(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	root := t.TempDir()

	content, digest, err := extension.Pack(taskmanagerPackage(), []extension.Asset{{
		Path: "assets/icon.svg", Content: []byte("<svg><!-- taskmanager --></svg>"),
	}})
	if err != nil {
		t.Fatalf("pack the system application: %v", err)
	}
	reference := "dist/kos-taskmanager-0.1.0.tar.gz"
	writePackage(t, root, reference, content)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(taskmanagerComponent, reference, digest)})

	installation := cluster.ExtensionState(t, extension.DirectoryArtifacts{Root: root})
	shell.Use(installation)
	result, found := installation.Component(taskmanagerComponent)
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want ready", result, found)
	}
	if result.Digest != digest {
		t.Fatalf("verdict digest = %q, want the artifact's digest %q", result.Digest, digest)
	}

	pages := listPageIDs(t, shell, owner)
	if !pages[taskmanagerPage] {
		t.Fatalf("pages = %v, want the package's page", pages)
	}
	items := listApps(t, shell, owner)
	app, found := appNamed(items, taskmanagerApp)
	if !found {
		t.Fatalf("applications = %v, want the package's window application", items)
	}
	for field, want := range map[string]string{
		"extension": taskmanagerExtension, "component": taskmanagerComponent,
		"phase": extension.PhaseReady, "digest": digest,
	} {
		if got := harness.String(app, field); got != want {
			t.Fatalf("application %s = %q, want %q", field, got, want)
		}
	}

	cluster.DeleteComponent(t, taskmanagerComponent)
	installation = cluster.ExtensionState(t, extension.DirectoryArtifacts{Root: root})
	shell.Use(installation)
	if pages := listPageIDs(t, shell, owner); pages[taskmanagerPage] {
		t.Fatalf("pages after the uninstall = %v, want the page gone", pages)
	}
	if items := listApps(t, shell, owner); len(items) != 0 {
		t.Fatalf("applications after the uninstall = %v, want none", items)
	}
}

// TestTC155DirectoryPackageRefusesADigestMismatch checks the negative face of
// KOSD-C-17 on the delivered channel: the bytes on disk are read and compared,
// and a mismatch names both digests instead of reporting an unreadable package.
func TestTC155DirectoryPackageRefusesADigestMismatch(t *testing.T) {
	cluster := harness.Start(t)
	root := t.TempDir()

	content, digest, err := extension.Pack(taskmanagerPackage(), nil)
	if err != nil {
		t.Fatalf("pack the system application: %v", err)
	}
	reference := "kos-taskmanager-0.1.0.tar.gz"
	writePackage(t, root, reference, content)
	named := "sha256:" + strings.Repeat("0", 64)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component(taskmanagerComponent, reference, named)})

	installation := cluster.ExtensionState(t, extension.DirectoryArtifacts{Root: root})
	result, found := installation.Component(taskmanagerComponent)
	if !found || result.Phase != extension.PhaseUnavailable || result.Reason != extension.ReasonDigestMismatch {
		t.Fatalf("component = %+v (found %v), want unavailable on a digest mismatch", result, found)
	}
	if !strings.Contains(result.Message, digest) || !strings.Contains(result.Message, named) {
		t.Fatalf("message = %q, want the carried and the named digest", result.Message)
	}
	if len(installation.Registry().Pages()) != 0 || len(installation.Registry().Apps()) != 0 {
		t.Fatalf("pages = %v apps = %v, want nothing from a refused package",
			installation.Registry().Pages(), installation.Registry().Apps())
	}

	condition := packageCondition(t, cluster, taskmanagerComponent)
	if condition.Status != metav1.ConditionFalse || condition.Reason != extension.ReasonDigestMismatch {
		t.Fatalf("PackageVerified = %s/%s, want False/%s", condition.Status, condition.Reason, extension.ReasonDigestMismatch)
	}
	if !strings.Contains(condition.Message, digest) || !strings.Contains(condition.Message, named) {
		t.Fatalf("PackageVerified message = %q, want both digests", condition.Message)
	}
}

// TestTC155DirectoryPackageReportsWhyItCannotBeRead checks a missing artifact
// and a reference that escapes the package directory are refused with a reason
// (C-17).
func TestTC155DirectoryPackageReportsWhyItCannotBeRead(t *testing.T) {
	cluster := harness.Start(t)
	root := t.TempDir()
	digest := "sha256:" + strings.Repeat("0", 64)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component("kos-missing", "kos-missing-0.1.0.tar.gz", digest)})
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Component("kos-escape", "../outside.tar.gz", digest)})

	installation := cluster.ExtensionState(t, extension.DirectoryArtifacts{Root: root})
	missing, _ := installation.Component("kos-missing")
	if missing.Phase != extension.PhaseUnavailable || missing.Reason != extension.ReasonPackageUnavailable {
		t.Fatalf("missing component = %s/%s, want unavailable", missing.Phase, missing.Reason)
	}
	if !strings.Contains(missing.Message, "is not available under") || !strings.Contains(missing.Message, root) {
		t.Fatalf("missing message = %q, want the reference and the package directory", missing.Message)
	}

	escape, _ := installation.Component("kos-escape")
	if escape.Phase != extension.PhaseUnavailable || escape.Reason != extension.ReasonPackageUnavailable {
		t.Fatalf("escape component = %s/%s, want unavailable", escape.Phase, escape.Reason)
	}
	if !strings.Contains(escape.Message, "not a path inside the package directory") {
		t.Fatalf("escape message = %q, want the containment refusal", escape.Message)
	}
	if len(installation.Registry().Pages()) != 0 || len(installation.Registry().Apps()) != 0 {
		t.Fatal("an unreadable package contributed a page or an application")
	}
}

// listPageIDs reads the pages the shell serves to one session (C-22).
func listPageIDs(t *testing.T, shell *harness.Shell, token string) map[string]bool {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list pages = %d %v", status, body)
	}
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("page list has no items: %v", body)
	}
	ids := make(map[string]bool, len(raw))
	for _, entry := range raw {
		page, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("page entry is not an object: %v", entry)
		}
		ids[harness.String(page, "id")] = true
	}
	return ids
}

// packageCondition reads one stored condition of a system component.
func packageCondition(t *testing.T, cluster *harness.Cluster, name string) metav1.Condition {
	t.Helper()
	conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "SystemComponent", name))
	for _, condition := range conditions {
		if condition.Type == extension.ConditionPackageVerified {
			return condition
		}
	}
	t.Fatalf("component %s carries no %s condition", name, extension.ConditionPackageVerified)
	return metav1.Condition{}
}

// writePackage writes one package file below root.
func writePackage(t *testing.T, root, reference string, content []byte) {
	t.Helper()
	file := filepath.Join(root, filepath.FromSlash(reference))
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatalf("make the package directory: %v", err)
	}
	if err := os.WriteFile(file, content, 0o644); err != nil {
		t.Fatalf("write the package file: %v", err)
	}
}
