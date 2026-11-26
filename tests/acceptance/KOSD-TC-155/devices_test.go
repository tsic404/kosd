package kosdtc155

import (
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	devicemanager "github.com/tsic404/kos-devicemanager/pkg/devicemanager"

	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/controlplane"
	"github.com/tsic404/kosd/pkg/extension"
	"github.com/tsic404/kosd/tests/acceptance/harness"
)

const (
	deviceComponent = "kos-devicemanager"
	deviceReference = "test://" + deviceComponent
	devicePage      = "devices"

	// deviceFresh is a node whose collection holds an occupied device and one
	// whose driver layer is missing; deviceUncollected never reported a fact;
	// deviceStale reported one beyond the refresh bound.
	deviceFresh       = "node-a"
	deviceUncollected = "node-b"
	deviceStale       = "node-c"
)

// TestTC155DeviceProductAnswersOnTheAssembledPanel installs the device product
// from the archive its own build produces and reads its page through the panel
// the production process assembles. No case-specific reader is registered:
// before the control plane carried the product's reader the same read answered
// 422 capability_missing (KOSD-AC-72, KOSD-TC-155, KOSD-C-53).
func TestTC155DeviceProductAnswersOnTheAssembledPanel(t *testing.T) {
	cluster := harness.Start(t)
	// No configure callback: the reader must come from the entry the production
	// process calls, never from a registration the case adds (KOSD-C-53).
	shell := cluster.StartShell(t)
	owner := shell.SignInOwner(t, "owner", "owner-password")
	clock := shell.Accounts.Clock.Now()

	// A permanently registered reader must not create the page by itself: before
	// the declaration exists the shell neither lists the device page nor serves
	// it (KOSD-C-53, DEVICE-TC-2).
	assertDevicePageWithdrawn(t, shell, owner, "before the install")

	deviceNode(t, cluster, deviceFresh)
	deviceNode(t, cluster, deviceUncollected)
	deviceNode(t, cluster, deviceStale)
	// Two distinct collection times, neither of them the read clock: node-a
	// collected six hours before the read, node-c thirty, so node-c's is the
	// oldest input and the one every item's observation rests on (DEVICE-C-1).
	freshAt := clock.Add(-6 * time.Hour)
	staleAt := clock.Add(-30 * time.Hour)
	cluster.Create(t, &unstructured.Unstructured{Object: deviceFact(deviceFresh, freshAt,
		deviceOf("gpu0", "gpu", true, true,
			map[string]any{"source": "device-plugin", "reference": "gpu-pool"}),
		deviceOf("nic0", "nic", true, false, nil),
	)})
	cluster.Create(t, &unstructured.Unstructured{Object: deviceFact(deviceStale, staleAt,
		deviceOf("nvme0", "disk", true, true, nil),
	)})

	artifacts := extension.MemoryArtifacts{}
	pkg, archive, err := devicemanager.Build(nil)
	if err != nil {
		t.Fatalf("build the device product package: %v", err)
	}
	if digest := harness.PackageDigest(t, archive); digest != pkg.Digest.String() {
		t.Fatalf("the kernel derives %s, the product published %s", digest, pkg.Digest)
	}
	installation := installArtifact(t, cluster, shell, deviceComponent, deviceReference,
		archive, pkg.Digest.String(), artifacts)
	result, found := installation.Component(deviceComponent)
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want the product ready", result, found)
	}
	// One product, one declaration: the page the shell serves is the four-item
	// page the package carries, not the legacy local copy's three items.
	if ids := pageIDs(t, shell, owner); len(ids) != 1 || ids[0] != devicePage {
		t.Fatalf("pages after the install = %v, want the device page alone", ids)
	}

	status, page := shell.Call(t, http.MethodGet, "/api/v1/pages/"+devicePage, nil, owner)
	if status != http.StatusOK {
		t.Fatalf("read the device page = %d %v, want %d; the assembled panel carries the product reader",
			status, page, http.StatusOK)
	}
	items := deviceItems(t, page)
	wantSources := map[string]string{
		devicemanager.ItemNodeSummary:   extension.PageSourceObject,
		devicemanager.ItemDeviceList:    extension.PageSourceObject,
		devicemanager.ItemLayerAlerts:   extension.PageSourceComputed,
		devicemanager.ItemFactStaleness: extension.PageSourceComputed,
	}
	// The raw array, not the index, is what the caller receives: an extra row
	// repeating a declared ID is a second displayed row and has to fail.
	if raw, _ := page["items"].([]any); len(raw) != len(wantSources) {
		t.Fatalf("the device page carries %d rows, want exactly the four the package declares: %v", len(raw), raw)
	}
	for id, source := range wantSources {
		item, known := items[id]
		if !known {
			t.Errorf("the device page is missing item %q", id)
			continue
		}
		if got := harness.String(item, "source"); got != source {
			t.Errorf("item %q source = %q, want %q", id, got, source)
		}
	}

	// Each item reports the moment its own source observed it, never the read
	// clock: the object-sourced items rest on the facts they read and the
	// computed items on their least fresh input, so every one of them is the
	// oldest collection time the fixtures state. A reader that stamped the read
	// clock would answer freshAt's or the read moment instead (KOSD-C-13,
	// DEVICE-C-1).
	oldestInput := staleAt.UTC().Format(time.RFC3339)
	wantObserved := map[string]string{
		devicemanager.ItemNodeSummary:   oldestInput,
		devicemanager.ItemDeviceList:    oldestInput,
		devicemanager.ItemLayerAlerts:   oldestInput,
		devicemanager.ItemFactStaleness: oldestInput,
	}
	for id, want := range wantObserved {
		item, known := items[id]
		if !known {
			continue
		}
		if got := harness.String(item, "observedAt"); got != want {
			t.Errorf("item %q observedAt = %q, want the collection time %q its source states", id, got, want)
		}
	}
	if page["readOnly"] != true || harness.String(page, "readOnlyReason") == "" {
		t.Fatalf("device page readOnly = %v reason = %q, want a stated read-only page",
			page["readOnly"], harness.String(page, "readOnlyReason"))
	}
	if actions, _ := page["actions"].([]any); len(actions) != 0 {
		t.Fatalf("read-only device page carries actions %v", page["actions"])
	}
	for id, item := range items {
		if item["entry"] != nil {
			t.Errorf("read-only item %q carries a modification entry", id)
		}
	}

	// The values rest on the facts the collector wrote, so a reader that only
	// echoed the declaration cannot pass: the occupied device, the missing
	// driver layer and the stale fact each have to appear.
	summaries := deviceListField(t, items, devicemanager.ItemNodeSummary, "nodes")
	if reason := deviceNodeReason(t, summaries, deviceUncollected); reason == "" {
		t.Fatalf("node summary = %v, want the uncollected node marked unobservable", summaries)
	}
	if collected := deviceNodeCollectedAt(t, summaries, deviceFresh); collected != freshAt.UTC().Format(time.RFC3339) {
		t.Fatalf("node summary = %v, want node-a's collection time %s", summaries, freshAt.UTC().Format(time.RFC3339))
	}
	inventory := deviceListField(t, items, devicemanager.ItemDeviceList, "nodes")
	if occupancy := deviceOccupancy(t, inventory, deviceFresh, "gpu0"); occupancy != "occupied" {
		t.Fatalf("device list = %v, want gpu0 occupied", inventory)
	}
	alerts := deviceListField(t, items, devicemanager.ItemLayerAlerts, "alerts")
	if !deviceAlertFor(t, alerts, deviceFresh, "nic0", "driverReady") {
		t.Fatalf("layer alerts = %v, want the missing driver layer of nic0", alerts)
	}
	stale := deviceListField(t, items, devicemanager.ItemFactStaleness, "devices")
	if !deviceStaleFor(t, stale, deviceStale, "nvme0", staleAt) {
		t.Fatalf("fact staleness = %v, want the stale nvme0 and the moment it was last observed", stale)
	}
	// A fresh fact must not be reported as stale: node-a's collection is inside
	// the bound, so only node-c belongs in this list (DEVICE-C-6).
	if deviceStaleFor(t, stale, deviceFresh, "gpu0", freshAt) {
		t.Fatalf("fact staleness = %v, want node-a's fresh collection left out", stale)
	}

	// The assembled read the report cites: every item the package declares with
	// its source type and the moment its source observed it (KOSD-C-53).
	for _, raw := range page["items"].([]any) {
		item, _ := raw.(map[string]any)
		t.Logf("device page item %s: source=%s observedAt=%s value=%v",
			harness.String(item, "id"), harness.String(item, "source"),
			harness.String(item, "observedAt"), item["value"])
	}

	// The page goes away with the component that declares it, while the reader
	// stays registered: a withdrawn declaration must not leave a page behind
	// (KOSD-C-53, DEVICE-TC-2).
	cluster.DeleteComponent(t, deviceComponent)
	shell.Use(cluster.ExtensionState(t, artifacts))
	assertDevicePageWithdrawn(t, shell, owner, "after the uninstall")
}

// assertDevicePageWithdrawn checks the device page is absent for one moment in
// the component's life: not listed, and refused when read directly as a page no
// active component declares (KOSD-C-53, DEVICE-TC-2).
func assertDevicePageWithdrawn(t *testing.T, shell *harness.Shell, owner, moment string) {
	t.Helper()
	for _, id := range pageIDs(t, shell, owner) {
		if id == devicePage {
			t.Fatalf("pages %s = %v, want no device page: a registered reader must not declare it", moment, pageIDs(t, shell, owner))
		}
	}
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+devicePage, nil, owner)
	if status != http.StatusNotFound {
		t.Fatalf("read the device page %s = %d %v, want %d for a page no component declares",
			moment, status, body, http.StatusNotFound)
	}
	if code := harness.String(body, "error", "code"); code != controlplane.CodeNotFound {
		t.Fatalf("read the device page %s = %q, want %q", moment, code, controlplane.CodeNotFound)
	}
}

// deviceNode writes one node the device page lists by name.
func deviceNode(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "KubeletReady",
				"lastHeartbeatTime":  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
				"lastTransitionTime": time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
			}},
		},
	})
}

// deviceFact writes the collection of one node: the object name is the node
// name, which is how the product pairs a fact with its node (NODE-C-70).
func deviceFact(node string, observedAt time.Time, devices ...any) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "NodeDevice",
		"metadata":   map[string]any{"name": node},
		"spec": map[string]any{
			"node":       node,
			"collector":  node,
			"observedAt": observedAt.UTC().Format(time.RFC3339),
			"devices":    devices,
		},
	}
}

// deviceOf is one recorded device with its four fact layers and its occupancy.
func deviceOf(name, kind string, recognized, driverReady bool, occupancy map[string]any) map[string]any {
	device := map[string]any{
		"name":        name,
		"serial":      "S-" + name,
		"model":       "Acceptance",
		"kind":        kind,
		"present":     true,
		"recognized":  recognized,
		"driverReady": driverReady,
		"published":   true,
	}
	if occupancy != nil {
		device["occupancy"] = occupancy
	}
	return device
}

// deviceItems indexes the rendered page's items.
func deviceItems(t *testing.T, page map[string]any) map[string]map[string]any {
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
		id := harness.String(item, "id")
		if _, duplicate := items[id]; duplicate {
			t.Fatalf("the device page carries item %q more than once: %v", id, raw)
		}
		items[id] = item
	}
	return items
}

// deviceListField reads one item's named list value.
func deviceListField(t *testing.T, items map[string]map[string]any, id, field string) []any {
	t.Helper()
	item, known := items[id]
	if !known {
		t.Fatalf("page has no item %q", id)
	}
	value, ok := item["value"].(map[string]any)
	if !ok {
		t.Fatalf("item %q value = %v, want an object", id, item["value"])
	}
	list, ok := value[field].([]any)
	if !ok {
		t.Fatalf("item %q value has no %q list: %v", id, field, value)
	}
	return list
}

// deviceEntryOf returns the entry of one node in a list keyed by node name.
func deviceEntryOf(list []any, node string) map[string]any {
	for _, entry := range list {
		object, ok := entry.(map[string]any)
		if ok && harness.String(object, "name") == node {
			return object
		}
	}
	return nil
}

// deviceNodeReason reports why one node's summary is unobservable.
func deviceNodeReason(t *testing.T, list []any, node string) string {
	t.Helper()
	entry := deviceEntryOf(list, node)
	if entry == nil {
		return ""
	}
	if entry["observable"] != false {
		return ""
	}
	return harness.String(entry, "reason")
}

// deviceNodeCollectedAt reports one node's collection time.
func deviceNodeCollectedAt(t *testing.T, list []any, node string) string {
	t.Helper()
	entry := deviceEntryOf(list, node)
	if entry == nil {
		return ""
	}
	return harness.String(entry, "collectedAt")
}

// deviceOccupancy reports the occupancy state one device carries.
func deviceOccupancy(t *testing.T, list []any, node, device string) string {
	t.Helper()
	entry := deviceEntryOf(list, node)
	if entry == nil {
		return ""
	}
	devices, _ := entry["devices"].([]any)
	for _, raw := range devices {
		record, ok := raw.(map[string]any)
		if !ok || harness.String(record, "name") != device {
			continue
		}
		return harness.String(record, "occupancy", "state")
	}
	return ""
}

// deviceAlertFor reports whether one device's layer is listed as missing.
func deviceAlertFor(t *testing.T, alerts []any, node, device, layer string) bool {
	t.Helper()
	for _, raw := range alerts {
		alert, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if harness.String(alert, "node") == node && harness.String(alert, "device") == device &&
			harness.String(alert, "layer") == layer {
			return true
		}
	}
	return false
}

// deviceStaleFor reports whether one device is listed as stale at the moment
// its collection states, so a fresh fact in the list does not pass.
func deviceStaleFor(t *testing.T, stale []any, node, device string, observedAt time.Time) bool {
	t.Helper()
	for _, raw := range stale {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if harness.String(entry, "node") == node && harness.String(entry, "device") == device &&
			harness.String(entry, "lastObservedAt") == observedAt.UTC().Format(time.RFC3339) {
			return true
		}
	}
	return false
}
