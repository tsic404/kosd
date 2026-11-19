package kosdtc155

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"

	kosdiagnostics "github.com/tsic404/kos-diagnostics"
)

const (
	diagnosticsComponent  = "kos-diagnostics"
	diagnosticsReference  = "test://" + diagnosticsComponent
	diagnosticsDefinition = "checkout"
	diagnosticsApp        = "checkout"
	diagnosticsVolume     = "data"
	undeclaredPage        = "no-such-page"

	probeComponent   = "diagnostics-probe"
	probeExtension   = "acme.diagnostics-probe"
	probePage        = "diagnostic-probe"
	probeRefusedPage = "diagnostic-probe-refused"
	probeUnrouted    = "diagnostic-probe-unrouted"
	probeItem        = kosdiagnostics.ItemDegradation
)

// diagnosticsAppOf seeds the stored state every diagnostics item reports: one
// application that failed and whose uninstall is stuck behind a volume.
func diagnosticsAppOf() map[string]any {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "Application",
		"metadata":   map[string]any{"name": diagnosticsApp, "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner":         "owner",
			"definitionRef": diagnosticsDefinition,
			"namespace":     harness.Namespace,
		},
		"status": map[string]any{
			"phase":  "Failed",
			"health": map[string]any{"state": string(v1beta1.HealthStateUnavailable)},
			"conditions": []any{
				map[string]any{
					"type": "Reconciled", "status": "False", "reason": "Failed",
					"message": "调和失败", "lastTransitionTime": now.Format(time.RFC3339),
				},
				map[string]any{
					"type": "Failed", "status": "True", "reason": "BackendFailed",
					"message": "后端不可用", "lastTransitionTime": now.Format(time.RFC3339),
				},
			},
			"uninstall": map[string]any{
				"blocked": []any{map[string]any{
					"kind": "Volume", "namespace": harness.Namespace, "name": diagnosticsVolume,
				}},
				"blockedSince": now.Add(-20 * time.Minute).UTC().Format(time.RFC3339),
				"stalled":      true,
			},
		},
	}
}

// diagnosticsProbe is a product the kernel does not ship: its pages borrow the
// diagnostics product's item references, so a rendered value proves the reader
// the panel registered is still in the dispatch table.
func diagnosticsProbe() extension.Declaration {
	page := func(id, title, itemID, reference string) extension.Page {
		return extension.Page{
			ID: id, Title: title, Location: "panel",
			ReadOnly: true, ReadOnlyReason: "只读展示",
			Items: []extension.PageItem{{
				ID: itemID, Label: title, Source: extension.PageSourceComputed, Reference: reference,
			}},
		}
	}
	return extension.Declaration{
		Identifier:       probeExtension,
		Version:          "1.0.0",
		FrameworkVersion: ">=1.1 <2.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{
				page(probePage, "诊断读取器探针", probeItem,
					kosdiagnostics.Identifier+"/"+kosdiagnostics.ItemDegradation),
				page(probeRefusedPage, "未声明条目探针", "unknown-item",
					kosdiagnostics.Identifier+"/no-such-item"),
				page(probeUnrouted, "未接入产品探针", "unrouted-item", "acme.absent/items"),
			},
		},
	}
}

// TestTC155DiagnosticsProductFollowsItsComponent installs the diagnostics
// product from the artifact its own SDK build produces, reads its page, then
// withdraws the component and reads the shell again (KOSD-AC-72, KOSD-TC-155).
func TestTC155DiagnosticsProductFollowsItsComponent(t *testing.T) {
	cluster := harness.Start(t)
	// No configure callback: the reader may only come from the entry the
	// production process calls, never from a case-specific registration.
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}
	kindsBefore := append([]string(nil), api.Kinds()...)

	cluster.InstallDefinition(t, harness.Definition(t, diagnosticsDefinition, nil))
	cluster.Create(t, &unstructured.Unstructured{Object: diagnosticsAppOf()})
	// A second product stays installed throughout, so the case witnesses what
	// the diagnostics install and uninstall touch (KOSD-C-28).
	installation := cluster.InstallPackageWith(t, otherComponent, otherProduct(), artifacts, shell.Retention())
	shell.Use(installation)
	if ids := pageIDs(t, shell, owner); !reflect.DeepEqual(ids, []string{otherComponent}) {
		t.Fatalf("pages before the install = %v, want the companion product alone", ids)
	}

	undeclaredStatus, undeclaredBody := diagnosticsAbsent(t, shell, owner, undeclaredPage)
	diagnosticsAssertAbsent(t, shell, owner, kosdiagnostics.PageDiagnostics, undeclaredStatus, undeclaredBody)

	pkg, archive, err := kosdiagnostics.Build(nil)
	if err != nil {
		t.Fatalf("build the diagnostics package: %v", err)
	}
	if digest := harness.PackageDigest(t, archive); digest != pkg.Digest.String() {
		t.Fatalf("the kernel derives %s, the SDK published %s", digest, pkg.Digest)
	}
	installation = installArtifact(t, cluster, shell, diagnosticsComponent, diagnosticsReference,
		archive, pkg.Digest.String(), artifacts)
	result, found := installation.Component(diagnosticsComponent)
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want ready", result, found)
	}
	if result.Digest != pkg.Digest.String() {
		t.Fatalf("verified digest = %q, want %q", result.Digest, pkg.Digest)
	}

	if ids := pageIDs(t, shell, owner); !reflect.DeepEqual(ids, []string{kosdiagnostics.PageDiagnostics, otherComponent}) {
		t.Fatalf("pages after the install = %v, want the diagnostics page beside the companion product", ids)
	}
	status, page := diagnosticsPage(t, shell, owner, kosdiagnostics.PageDiagnostics)
	if status != http.StatusOK {
		t.Fatalf("read the diagnostics page = %d %v, want %d", status, page, http.StatusOK)
	}
	items := diagnosticsItems(t, page)
	if len(items) != 4 {
		t.Fatalf("the diagnostics page has %d items, want the four the declaration names", len(items))
	}
	degradation := diagnosticsValue(t, items, kosdiagnostics.ItemDegradation)
	if !diagnosticsNames(degradation, "reasons", diagnosticsApp) {
		t.Fatalf("degradation = %v, want the failed application", degradation)
	}
	blocked := diagnosticsValue(t, items, kosdiagnostics.ItemBlocked)
	if !diagnosticsNames(blocked, "objects", diagnosticsApp) {
		t.Fatalf("blocked objects = %v, want the stalled uninstall", blocked)
	}
	evidence := diagnosticsValue(t, items, kosdiagnostics.ItemEvidence)
	if !diagnosticsNames(evidence, "applications", diagnosticsApp) {
		t.Fatalf("evidence = %v, want the application's chain", evidence)
	}
	export := diagnosticsValue(t, items, kosdiagnostics.ItemExport)
	if !diagnosticsNames(export, "applications", diagnosticsApp) {
		t.Fatalf("export targets = %v, want the application", export)
	}
	entry, ok := items[kosdiagnostics.ItemExport]["entry"].(map[string]any)
	if !ok || harness.String(entry, "method") != http.MethodPost ||
		harness.String(entry, "path") != kosdiagnostics.PathBundles {
		t.Fatalf("export entry = %v, want %s %s",
			items[kosdiagnostics.ItemExport]["entry"], http.MethodPost, kosdiagnostics.PathBundles)
	}
	actions, _ := page["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("page actions = %v, want the export action alone", page["actions"])
	}
	action, _ := actions[0].(map[string]any)
	if harness.String(action, "id") != kosdiagnostics.ActionExport ||
		harness.String(action, "entry", "path") != kosdiagnostics.PathBundles {
		t.Fatalf("action = %v, want the export write", action)
	}

	cluster.DeleteComponent(t, diagnosticsComponent)
	installation = cluster.ExtensionState(t, artifacts)
	shell.Use(installation)

	if ids := pageIDs(t, shell, owner); !reflect.DeepEqual(ids, []string{otherComponent}) {
		t.Fatalf("pages after the uninstall = %v, want the companion product alone", ids)
	}
	diagnosticsAssertAbsent(t, shell, owner, kosdiagnostics.PageDiagnostics, undeclaredStatus, undeclaredBody)

	// The reader is a property of the assembled panel, not of the component:
	// reinstalling the same artifact brings the page back without a restart or
	// a new registration.
	installation = installArtifact(t, cluster, shell, diagnosticsComponent, diagnosticsReference,
		archive, pkg.Digest.String(), artifacts)
	if ids := pageIDs(t, shell, owner); !reflect.DeepEqual(ids, []string{kosdiagnostics.PageDiagnostics, otherComponent}) {
		t.Fatalf("pages after the reinstall = %v, want the page back beside the companion product", ids)
	}

	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
	if phase := cluster.ApplicationPhase(t, diagnosticsApp); phase != "Failed" {
		t.Fatalf("application phase after the cycle = %q, want the seeded failure untouched", phase)
	}
	registry := installation.Registry()
	if classes := registry.ObjectClasses(); len(classes) != 0 {
		t.Fatalf("extension object classes = %+v, want none", classes)
	}
	if capabilities := registry.ExtensionCapabilities(); len(capabilities) != 0 {
		t.Fatalf("extension capabilities = %+v, want none", capabilities)
	}
	if pages := registry.Pages(); len(pages) != 2 {
		t.Fatalf("pages in the projection = %+v, want the two products", pages)
	}
	if app := registry.AppsOf(otherProduct().Identifier); len(app) != 1 {
		t.Fatalf("the companion product's applications = %+v, want its window intact", app)
	}
}

// TestTC155DiagnosticsReaderOutlivesTheProduct proves the reader stays
// registered while the diagnostics page exists only through a declaration: a
// page another product declares still resolves the diagnostics items, and an
// item no product answers is refused rather than given a value
// (KOSD-C-22, KOSD-C-28, KOSD-C-29).
func TestTC155DiagnosticsReaderOutlivesTheProduct(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}
	cluster.InstallDefinition(t, harness.Definition(t, diagnosticsDefinition, nil))
	cluster.Create(t, &unstructured.Unstructured{Object: diagnosticsAppOf()})

	installation := cluster.InstallPackageWith(t, probeComponent, diagnosticsProbe(), artifacts, shell.Retention())
	shell.Use(installation)
	wantPages := []string{probePage, probeRefusedPage, probeUnrouted}
	if ids := pageIDs(t, shell, owner); !reflect.DeepEqual(ids, wantPages) {
		t.Fatalf("pages = %v, want the probe pages alone", ids)
	}

	// The borrowed reference reaches the product reader the panel registered,
	// although no diagnostics component is installed.
	status, page := diagnosticsPage(t, shell, owner, probePage)
	if status != http.StatusOK {
		t.Fatalf("read the probe page = %d %v, want %d", status, page, http.StatusOK)
	}
	value := diagnosticsValue(t, diagnosticsItems(t, page), probeItem)
	if !diagnosticsNames(value, "reasons", diagnosticsApp) {
		t.Fatalf("probe value = %v, want the product reader's degradation reasons", value)
	}

	// An item the product does not declare is refused with the product's own
	// answer, not with a fabricated value.
	status, body := diagnosticsPage(t, shell, owner, probeRefusedPage)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("read an item the product does not declare = %d %v, want %d",
			status, body, http.StatusUnprocessableEntity)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeCapabilityMissing {
		t.Fatalf("code = %q, want %q", code, controlplane.CodeCapabilityMissing)
	}

	// An item whose reference names no routed product is refused the same way,
	// whether or not another product is active.
	status, body = diagnosticsPage(t, shell, owner, probeUnrouted)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("read an item no product routes = %d %v, want %d",
			status, body, http.StatusUnprocessableEntity)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeCapabilityMissing {
		t.Fatalf("code = %q, want %q", code, controlplane.CodeCapabilityMissing)
	}

	// Nothing declared the diagnostics page, so it stays absent.
	undeclaredStatus, undeclaredBody := diagnosticsAbsent(t, shell, owner, undeclaredPage)
	diagnosticsAssertAbsent(t, shell, owner, kosdiagnostics.PageDiagnostics, undeclaredStatus, undeclaredBody)
}

// diagnosticsPage reads one page projection.
func diagnosticsPage(t *testing.T, shell *harness.Shell, token, id string) (int, map[string]any) {
	t.Helper()
	return shell.Call(t, http.MethodGet, "/api/v1/pages/"+id, nil, token)
}

// diagnosticsAbsent reads a page no active component declares; its answer is
// the baseline a withdrawn page must match.
func diagnosticsAbsent(t *testing.T, shell *harness.Shell, token, id string) (int, map[string]any) {
	t.Helper()
	status, body := diagnosticsPage(t, shell, token, id)
	if status != http.StatusNotFound {
		t.Fatalf("read the undeclared page %q = %d %v, want %d", id, status, body, http.StatusNotFound)
	}
	if _, present := body["items"]; present {
		t.Fatalf("the undeclared page %q carries page data: %v", id, body)
	}
	return status, body
}

// diagnosticsAssertAbsent checks one page answers exactly like a page no
// component declares, so a withdrawn page discloses nothing.
func diagnosticsAssertAbsent(t *testing.T, shell *harness.Shell, token, id string, wantStatus int, want map[string]any) {
	t.Helper()
	status, body := diagnosticsAbsent(t, shell, token, id)
	if status != wantStatus {
		t.Fatalf("page %q = %d, an undeclared page = %d", id, status, wantStatus)
	}
	for _, field := range []string{"code", "message", "stage"} {
		got, expected := harness.String(body, "error", field), harness.String(want, "error", field)
		if got != expected {
			t.Fatalf("page %q error.%s = %q, an undeclared page says %q", id, field, got, expected)
		}
	}
}

// diagnosticsItems indexes one rendered page's items by identity.
func diagnosticsItems(t *testing.T, page map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := page["items"].([]any)
	if !ok {
		t.Fatalf("page items = %v, want an array", page["items"])
	}
	items := make(map[string]map[string]any, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("page item is not an object: %v", entry)
		}
		items[harness.String(item, "id")] = item
	}
	return items
}

// diagnosticsValue returns one server item's value, failing when the shell
// served none or stamped no observation time (C-13).
func diagnosticsValue(t *testing.T, items map[string]map[string]any, id string) map[string]any {
	t.Helper()
	item, known := items[id]
	if !known {
		t.Fatalf("page has no item %q", id)
	}
	if harness.String(item, "observedAt") == "" {
		t.Fatalf("item %q carries no observation time (C-13)", id)
	}
	value, ok := item["value"].(map[string]any)
	if !ok {
		t.Fatalf("item %q value = %v, want an object", id, item["value"])
	}
	return value
}

// diagnosticsNames reports whether one list field of a value names an object.
func diagnosticsNames(container map[string]any, field, name string) bool {
	entries, _ := container[field].([]any)
	for _, entry := range entries {
		object, ok := entry.(map[string]any)
		if ok && harness.String(object, "name") == name {
			return true
		}
	}
	return false
}
