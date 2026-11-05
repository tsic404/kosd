// Package device implements the control-plane side of the device fact
// contract: the validator marks facts that fall outside the collection
// boundary, a maintainer refreshes the validity markers and reclaims a vanished
// node's fact, and the read side builds the device view.
//
// The collection face — reading a node's hardware and writing its fact — lives
// in tsic404/kos-node. This package owns the fact object's boundary rules, its
// validity markers and the read view (C-37).
package device

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kosd/api"
	"github.com/tsic404/kosd/api/v1beta1"
	"github.com/tsic404/kosd/pkg/markers"
	"github.com/tsic404/kosd/pkg/write"
)

const (
	// StaleAfter is how long a device fact may go unrefreshed before KOS marks
	// it stale (C-37).
	StaleAfter = 24 * time.Hour
	// ReclaimAfter is how long the fact of a vanished node is kept before KOS
	// reclaims it (C-37).
	ReclaimAfter = 7 * 24 * time.Hour
	// markerValue is the value a validity marker carries.
	markerValue = "true"
)

// nodeDeviceResource is the class the facts live in.
var nodeDeviceResource = schema.GroupVersionResource{
	Group:    api.Group,
	Version:  v1beta1.GroupVersion.Version,
	Resource: "nodedevices",
}

// Validator writes the validity markers of C-37. It is the subject the write
// allocation grants the validity marker class to.
type Validator struct {
	// Writer executes the marker writes.
	Writer *write.Writer
}

// MarkInvalid marks a device fact invalid. The mark is sticky: a fact that was
// written outside the boundary does not become trusted again by a later write.
func (v *Validator) MarkInvalid(ctx context.Context, ref write.Ref) error {
	return v.mark(ctx, ref, markers.Invalid, true)
}

// MarkStale marks a device fact as no longer refreshed.
func (v *Validator) MarkStale(ctx context.Context, ref write.Ref) error {
	return v.mark(ctx, ref, markers.Stale, true)
}

// ClearStale removes the stale mark of a fact that is refreshed again.
func (v *Validator) ClearStale(ctx context.Context, ref write.Ref) error {
	return v.mark(ctx, ref, markers.Stale, false)
}

// CheckBoundary checks the stored boundary rule: the object's node field must
// equal its name. A violation marks the object invalid (C-37); the boolean says
// whether one was found.
func (v *Validator) CheckBoundary(ctx context.Context, object *unstructured.Unstructured) (bool, error) {
	node, _, err := unstructured.NestedString(object.Object, "spec", "node")
	if err != nil {
		return false, fmt.Errorf("read node field of %s: %w", object.GetName(), err)
	}
	if node == object.GetName() {
		return false, nil
	}
	return true, v.MarkInvalid(ctx, write.RefOf(object))
}

// mark sets or clears one validity marker, writing only when the stored value
// differs so repeated rounds produce no write (C-06).
func (v *Validator) mark(ctx context.Context, ref write.Ref, key string, present bool) error {
	if v.Writer == nil {
		return errors.New("device: a writer is required")
	}
	current, err := v.Writer.Get(ctx, ref)
	if err != nil {
		return err
	}
	_, hasMarker := current.GetLabels()[key]
	if hasMarker == present {
		return nil
	}
	set := map[string]string{}
	var remove []string
	if present {
		set[key] = markerValue
	} else {
		remove = []string{key}
	}
	_, err = v.Writer.MarkOn(ctx, write.SubjectValidator, write.Session{}, current, set, remove)
	return err
}
