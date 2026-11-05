package device_test

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kosd/pkg/device"
)

// TestViewDoesNotInferLayers covers the rule that one missing layer does not
// imply another and that such a device is not presented as free (C-37).
func TestViewDoesNotInferLayers(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "NodeDevice",
		"metadata":   map[string]any{"name": "node-a", "labels": map[string]any{}},
		"spec": map[string]any{
			"node":       "node-a",
			"observedAt": "2026-10-25T00:00:00Z",
			"devices": []any{map[string]any{
				"name": "gpu0", "present": true, "recognized": true, "driverReady": true, "published": false,
			}},
		},
	}}
	view := device.BuildView([]*unstructured.Unstructured{object}, time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC))
	if len(view.Devices) != 1 {
		t.Fatalf("view shows %d devices, want 1", len(view.Devices))
	}
	presented := view.Devices[0]
	if presented.IsPublished {
		t.Fatal("published layer = true, want false")
	}
	if presented.IsAllocatable {
		t.Fatal("a device whose published layer does not hold is presented as allocatable")
	}
}
